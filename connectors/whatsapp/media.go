package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// mediaRef is what whatsmeow needs to download and decrypt a file again. It
// serves Download as a DownloadableMessage.
type mediaRef struct {
	Kind          whatsmeow.MediaType `json:"kind"`
	DirectPath    string              `json:"direct_path"`
	MediaKey      []byte              `json:"media_key"`
	FileSHA256    []byte              `json:"file_sha256"`
	FileEncSHA256 []byte              `json:"file_enc_sha256"`
}

func (r *mediaRef) GetDirectPath() string             { return r.DirectPath }
func (r *mediaRef) GetMediaKey() []byte               { return r.MediaKey }
func (r *mediaRef) GetFileSHA256() []byte             { return r.FileSHA256 }
func (r *mediaRef) GetFileEncSHA256() []byte          { return r.FileEncSHA256 }
func (r *mediaRef) GetMediaType() whatsmeow.MediaType { return r.Kind }

// attachment is m's file, if it has one WhatsApp's servers hold.
func attachment(m *waE2E.Message) (store.Media, bool) {
	var f interface {
		whatsmeow.DownloadableMessage
		GetMimetype() string
		GetFileLength() uint64
	}
	var kind whatsmeow.MediaType
	var name string
	switch {
	case m.ImageMessage != nil:
		f, kind = m.ImageMessage, whatsmeow.MediaImage
	case m.VideoMessage != nil:
		f, kind = m.VideoMessage, whatsmeow.MediaVideo
	case m.PtvMessage != nil:
		f, kind = m.PtvMessage, whatsmeow.MediaVideo
	case m.AudioMessage != nil:
		f, kind = m.AudioMessage, whatsmeow.MediaAudio
	case m.DocumentMessage != nil:
		f, kind, name = m.DocumentMessage, whatsmeow.MediaDocument, m.DocumentMessage.GetFileName()
	case m.StickerMessage != nil:
		f, kind = m.StickerMessage, whatsmeow.MediaImage
	default:
		return store.Media{}, false
	}
	if f.GetDirectPath() == "" || len(f.GetMediaKey()) == 0 {
		return store.Media{}, false
	}
	ref, err := json.Marshal(mediaRef{
		Kind: kind, DirectPath: f.GetDirectPath(), MediaKey: f.GetMediaKey(),
		FileSHA256: f.GetFileSHA256(), FileEncSHA256: f.GetFileEncSHA256(),
	})
	if err != nil {
		return store.Media{}, false
	}
	return store.Media{
		Type: cmp.Or(f.GetMimetype(), "application/octet-stream"),
		Name: name,
		Size: int64(f.GetFileLength()),
		Ref:  ref,
	}, true
}

// Media downloads a stored message's file from WhatsApp's servers and
// decrypts it.
func (c *waConnector) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	item, err := c.st.Media(ctx, message, index)
	if errors.Is(err, store.ErrNoMedia) {
		return connector.MediaFile{}, &connector.Error{
			Status:  http.StatusNotFound,
			Message: "no file to fetch: polls, locations and contacts have none, and media is not available for messages stored before mandate could fetch it",
		}
	}
	if err != nil {
		return connector.MediaFile{}, err
	}
	var ref mediaRef
	if err := json.Unmarshal(item.Ref, &ref); err != nil {
		return connector.MediaFile{}, fmt.Errorf("media reference of %s: %w", message, err)
	}
	cli, _, _ := c.current()
	if !cli.IsConnected() || !cli.IsLoggedIn() {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "whatsapp is not connected"}
	}
	file, size, err := download(ctx, cli, &ref)
	var tooLarge *connector.Error
	switch {
	case errors.As(err, &tooLarge):
		return connector.MediaFile{}, err
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404),
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return connector.MediaFile{}, &connector.Error{Status: http.StatusNotFound, Message: "WhatsApp's servers no longer have this file; they keep media only for a while"}
	case err != nil:
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from WhatsApp: " + err.Error()}
	}
	return connector.MediaFile{Type: item.Type, Name: item.Name, Size: size, Body: file}, nil
}

// AES-CBC padding and the MAC that whatsmeow strips after downloading.
const encryptionOverhead = 16 + 10

var errOverLimit = errors.New("file over the media limit")

// download has whatsmeow fetch ref into an unlinked temporary file and
// decrypt it there, so the file is held on disk, and only up to the media
// limit: writes past it fail and cancel the download.
func download(ctx context.Context, cli waClient, ref *mediaRef) (*os.File, int64, error) {
	limit := connector.MediaLimit(ctx)
	f, err := os.CreateTemp("", "media-")
	if err != nil {
		return nil, 0, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	capped := &cappedFile{f: f, max: limit + encryptionOverhead, cancel: cancel}
	err = cli.DownloadToFile(ctx, ref, capped)
	var size int64
	if err == nil {
		var fi os.FileInfo
		if fi, err = f.Stat(); err == nil {
			size = fi.Size()
			_, err = f.Seek(0, io.SeekStart)
		}
	}
	switch {
	case capped.over || err == nil && size > limit:
		err = connector.TooLarge(limit)
	case err == nil:
		return f, size, nil
	}
	f.Close()
	return nil, 0, err
}

// cappedFile is the whatsmeow.File it downloads into. It does not embed
// *os.File: io.Copy would use its ReadFrom past Write, and whatsmeow would
// preallocate the server's Content-Length.
type cappedFile struct {
	f      *os.File
	max    int64
	over   bool
	cancel context.CancelFunc
}

func (c *cappedFile) Read(p []byte) (int, error)                { return c.f.Read(p) }
func (c *cappedFile) ReadAt(p []byte, off int64) (int, error)   { return c.f.ReadAt(p, off) }
func (c *cappedFile) Seek(off int64, whence int) (int64, error) { return c.f.Seek(off, whence) }
func (c *cappedFile) Stat() (os.FileInfo, error)                { return c.f.Stat() }

func (c *cappedFile) Write(p []byte) (int, error) {
	off, err := c.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if err := c.check(off + int64(len(p))); err != nil {
		return 0, err
	}
	return c.f.Write(p)
}

func (c *cappedFile) WriteAt(p []byte, off int64) (int, error) {
	if err := c.check(off + int64(len(p))); err != nil {
		return 0, err
	}
	return c.f.WriteAt(p, off)
}

func (c *cappedFile) Truncate(size int64) error {
	if err := c.check(size); err != nil {
		return err
	}
	return c.f.Truncate(size)
}

func (c *cappedFile) check(end int64) error {
	if end > c.max {
		c.over = true
		c.cancel()
		return errOverLimit
	}
	return nil
}
