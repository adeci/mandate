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
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// mediaRef is what fetching a file needs: its mxc:// URI in plain rooms, or
// in encrypted rooms the file object with its URI, key and hash.
type mediaRef struct {
	URL  id.ContentURIString      `json:"url,omitempty"`
	File *event.EncryptedFileInfo `json:"file,omitempty"`
}

var errNoFile = &connector.Error{Status: http.StatusNotFound, Message: "no file to fetch: this message has none, or the homeserver no longer has it"}

// attachmentOf is the file of an image, video, audio, file or sticker message.
func attachmentOf(t event.Type, content *event.MessageEventContent) (store.Media, bool) {
	var name string
	switch {
	case t == event.EventSticker:
	case content.MsgType == event.MsgImage, content.MsgType == event.MsgVideo,
		content.MsgType == event.MsgAudio, content.MsgType == event.MsgFile:
		// Without a filename, the body is the file's name.
		name = cmp.Or(content.FileName, content.Body)
	default:
		return store.Media{}, false
	}
	ref := mediaRef{URL: content.URL, File: content.File}
	if ref.File != nil {
		ref.URL = ""
		if ref.File.URL == "" {
			return store.Media{}, false
		}
	} else if ref.URL == "" {
		return store.Media{}, false
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return store.Media{}, false
	}
	m := store.Media{Type: "application/octet-stream", Name: name, Ref: b}
	if content.Info != nil {
		m.Type = cmp.Or(content.Info.MimeType, m.Type)
		m.Size = int64(max(content.Info.Size, 0))
	}
	return m, true
}

// Media downloads a message's file from the homeserver and, in encrypted
// rooms, decrypts it. Messages stored without a reference are read again
// from the room.
func (c *mxConnector) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	a := c.use()
	if a == nil {
		return connector.MediaFile{}, errNotLinked
	}
	defer a.release()
	item, err := c.st.Media(ctx, message, index)
	if errors.Is(err, store.ErrNoMedia) {
		if index != 0 {
			return connector.MediaFile{}, errNoFile
		}
		item, err = c.refetch(ctx, a, message)
	}
	if err != nil {
		return connector.MediaFile{}, err
	}
	var ref mediaRef
	if err := json.Unmarshal(item.Ref, &ref); err != nil {
		return connector.MediaFile{}, fmt.Errorf("media reference of %s: %w", message, err)
	}
	uri := ref.URL
	if ref.File != nil {
		uri = ref.File.URL
	}
	mxc, err := uri.Parse()
	if err != nil {
		return connector.MediaFile{}, fmt.Errorf("media reference of %s: %w", message, err)
	}
	resp, err := a.cli.Download(ctx, mxc)
	if errors.Is(err, mautrix.MNotFound) {
		return connector.MediaFile{}, errNoFile
	} else if err != nil {
		return connector.MediaFile{}, upstream("download the file", err)
	}
	file := connector.MediaFile{Type: item.Type, Name: item.Name, Size: resp.ContentLength, Body: resp.Body}
	if ref.File == nil && resp.ContentLength >= 0 {
		return file, nil
	}
	// An encrypted file is checked against its hash before any of it is
	// passed on.
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return connector.MediaFile{}, upstream("download the file", err)
	}
	if ref.File != nil {
		if err := ref.File.DecryptInPlace(data); errors.Is(err, attachment.ErrHashMismatch) {
			return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "the homeserver sent a file that does not match its hash"}
		} else if err != nil {
			return connector.MediaFile{}, fmt.Errorf("decrypt the file of %s: %w", message, err)
		}
	}
	file.Size, file.Body = int64(len(data)), io.NopCloser(bytes.NewReader(data))
	return file, nil
}

// refetch reads message from its room again for the file it carries.
func (c *mxConnector) refetch(ctx context.Context, a *account, message string) (store.Media, error) {
	var room string
	err := c.st.DB().QueryRowContext(ctx, `SELECT container_id FROM messages WHERE id = ?1`, message).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Media{}, errNoFile
	} else if err != nil {
		return store.Media{}, err
	}
	evt, err := a.cli.GetEvent(ctx, id.RoomID(room), id.EventID(message))
	if errors.Is(err, mautrix.MNotFound) {
		return store.Media{}, errNoFile
	} else if err != nil {
		return store.Media{}, upstream("read the message", err)
	}
	prepare(evt, id.RoomID(room))
	if evt.Type == event.EventEncrypted {
		if evt, err = a.mach.DecryptMegolmEvent(ctx, evt); err != nil {
			return store.Media{}, &connector.Error{Status: http.StatusNotFound, Message: "the message cannot be decrypted yet, so its file cannot be fetched"}
		}
	}
	if m, ok := attachmentOf(evt.Type, evt.Content.AsMessage()); ok {
		return m, nil
	}
	return store.Media{}, errNoFile
}
