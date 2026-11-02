package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// refetchMessages is how many messages up to a stored one are asked for to
// find it again on the phone.
const refetchMessages = 10

const (
	noFile        = "no file to fetch: the phone no longer has this message, or it has no file at this index"
	notDownloaded = "the phone has not downloaded this file yet; open the message in Google Messages on the phone, then try again"
)

// mediaRef is what libgm needs to download and decrypt a file from Google's
// servers. ID is empty until the phone has uploaded the whole file, which
// for an MMS waits for the phone to download it; a preview may be there
// before. Data holds a file sent whole within the message.
type mediaRef struct {
	ID       string `json:"id,omitempty"`
	Key      []byte `json:"key,omitempty"`
	ThumbID  string `json:"thumb_id,omitempty"`
	ThumbKey []byte `json:"thumb_key,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// whole reports whether r serves the file of the given size itself. The
// data within a message is often only a small preview.
func (r mediaRef) whole(size int64) bool {
	return r.ID != "" || r.Data != nil && (size == 0 || int64(len(r.Data)) == size)
}

func attachment(mc *gmproto.MediaContent) store.Media {
	ref := mediaRef{
		ID: mc.GetMediaID(), Key: mc.GetDecryptionKey(),
		ThumbID: mc.GetThumbnailMediaID(), ThumbKey: mc.GetThumbnailDecryptionKey(),
	}
	size := mc.GetSize()
	if inline := (mediaRef{Data: mc.GetMediaData()}); ref.ID == "" && inline.whole(size) {
		ref.Data = inline.Data
		size = int64(len(ref.Data))
	}
	data, _ := json.Marshal(ref) // a struct of strings and bytes
	return store.Media{
		Type: cmp.Or(libgm.FormatToMediaType[mc.GetFormat()].Format, mc.GetMimeType(), "application/octet-stream"),
		Name: fileName(mc.GetMediaName()),
		Size: size,
		Ref:  data,
	}
}

// fileName drops what Google gives in place of a file name, such as a part
// number, so a name without an extension counts as none.
func fileName(name string) string {
	if filepath.Ext(name) == "" {
		return ""
	}
	return name
}

// Media downloads a stored message's file and decrypts it. A message stored
// without the whole file's reference is looked up on the phone again; if the
// phone has only a preview so far, the preview is served, named as one.
func (c *gmConnector) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	cli, h := c.state()
	if cli == nil {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "google messages is not linked"}
	}
	item, err := c.st.Media(ctx, message, index)
	if err != nil && !errors.Is(err, store.ErrNoMedia) {
		return connector.MediaFile{}, err
	}
	var ref mediaRef
	if err == nil {
		if err := json.Unmarshal(item.Ref, &ref); err != nil {
			return connector.MediaFile{}, fmt.Errorf("media reference of %s: %w", message, err)
		}
	}
	if !ref.whole(item.Size) {
		if h.State != connector.Healthy {
			return connector.MediaFile{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "google messages is not connected: " + h.Detail}
		}
		if item, ref, err = c.refetch(ctx, cli, message, index); err != nil {
			return connector.MediaFile{}, err
		}
	}
	file := connector.MediaFile{Type: item.Type, Name: fileName(item.Name)}
	var data []byte
	switch {
	case ref.ID != "":
		data, err = download(cli, ref.ID, ref.Key)
	case ref.whole(item.Size):
		data = ref.Data
	default:
		data, err = download(cli, ref.ThumbID, ref.ThumbKey)
		// A video's preview is an image, so its type is the preview's own,
		// and the name leaves the extension to it.
		file.Type, _, _ = mime.ParseMediaType(http.DetectContentType(data))
		file.Name = "preview"
	}
	if err != nil {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Google Messages: " + err.Error()}
	}
	file.Size, file.Body = int64(len(data)), io.NopCloser(bytes.NewReader(data))
	return file, nil
}

// libgm reports the size the sender gave, which the decrypted stream need
// not match, so the file is read whole.
func download(cli gmClient, id string, key []byte) ([]byte, error) {
	body, err := cli.DownloadMedia(id, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// refetch asks the phone for the messages up to message, stores what it
// says about it, and returns its file at index: the whole file, or else a
// preview.
func (c *gmConnector) refetch(ctx context.Context, cli gmClient, message string, index int) (store.Media, mediaRef, error) {
	var chat, sent string
	err := c.st.DB().QueryRowContext(ctx, `SELECT container_id, sent_at FROM messages WHERE id = ?1`, message).Scan(&chat, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Media{}, mediaRef{}, &connector.Error{Status: http.StatusNotFound, Message: noFile}
	}
	if err != nil {
		return store.Media{}, mediaRef{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, sent)
	if err != nil {
		return store.Media{}, mediaRef{}, fmt.Errorf("sent_at of %s: %w", message, err)
	}
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	// The cursor asks for the messages before it; sent_at keeps milliseconds.
	resp, err := cli.FetchMessages(lctx, chat, refetchMessages, &gmproto.Cursor{LastItemID: message, LastItemTimestamp: at.UnixMilli() + 1})
	switch {
	case errors.Is(err, libgm.ErrPhoneNotResponding), errors.Is(err, context.DeadlineExceeded):
		return store.Media{}, mediaRef{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "the phone did not answer; it needs to be on and online"}
	case err != nil:
		return store.Media{}, mediaRef{}, &connector.Error{Status: http.StatusBadGateway, Message: "look up the message on the phone: " + err.Error()}
	}
	for _, m := range resp.GetMessages() {
		if m.GetMessageID() != message {
			continue
		}
		conv, _ := c.in.conversation(chat)
		if o, ok := convert(m, conv); ok {
			if err := c.in.apply(ctx, o); err != nil {
				c.log.Error("store refetched message", "id", message, "err", err)
			}
		}
		_, files := content(m)
		if index < len(files) {
			var ref mediaRef
			if err := json.Unmarshal(files[index].Ref, &ref); err != nil {
				return store.Media{}, mediaRef{}, err
			}
			if ref.whole(files[index].Size) || ref.ThumbID != "" {
				return files[index], ref, nil
			}
		}
		if index < len(files) || awaitingDownload(m.GetMessageStatus().GetStatus()) {
			return store.Media{}, mediaRef{}, &connector.Error{Status: http.StatusNotFound, Message: notDownloaded}
		}
		break
	}
	return store.Media{}, mediaRef{}, &connector.Error{Status: http.StatusNotFound, Message: noFile}
}
