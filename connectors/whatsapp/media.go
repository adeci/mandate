package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
	data, err := cli.Download(ctx, &ref)
	switch {
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404),
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return connector.MediaFile{}, &connector.Error{Status: http.StatusNotFound, Message: "WhatsApp's servers no longer have this file; they keep media only for a while"}
	case err != nil:
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from WhatsApp: " + err.Error()}
	}
	return connector.MediaFile{Type: item.Type, Name: item.Name, Size: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
}
