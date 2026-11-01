package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// mediaRef is the part of an attachment pointer needed to download and
// decrypt the file from Signal's CDN.
type mediaRef struct {
	CdnNumber   uint32 `json:"cdn_number"`
	CdnID       uint64 `json:"cdn_id,omitempty"`
	CdnKey      string `json:"cdn_key,omitempty"`
	Key         []byte `json:"key"`
	Digest      []byte `json:"digest"`
	Size        uint32 `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	FileName    string `json:"file_name,omitempty"`
}

func (r *mediaRef) pointer() *signalpb.AttachmentPointer {
	p := &signalpb.AttachmentPointer{
		CdnNumber: &r.CdnNumber, Key: r.Key, Digest: r.Digest, Size: &r.Size,
	}
	if r.CdnID != 0 {
		p.AttachmentIdentifier = &signalpb.AttachmentPointer_CdnId{CdnId: r.CdnID}
	} else {
		p.AttachmentIdentifier = &signalpb.AttachmentPointer_CdnKey{CdnKey: r.CdnKey}
	}
	return p
}

func attachment(a *signalpb.AttachmentPointer) (store.Media, bool) {
	if len(a.GetKey()) != 64 || len(a.GetDigest()) == 0 || (a.GetCdnId() == 0 && a.GetCdnKey() == "") {
		return store.Media{}, false
	}
	ct := a.GetContentType()
	if ct == "" {
		ct = "application/octet-stream"
	}
	ref, err := json.Marshal(mediaRef{
		CdnNumber: a.GetCdnNumber(), CdnID: a.GetCdnId(), CdnKey: a.GetCdnKey(),
		Key: a.GetKey(), Digest: a.GetDigest(), Size: a.GetSize(),
		ContentType: ct, FileName: a.GetFileName(),
	})
	if err != nil {
		return store.Media{}, false
	}
	return store.Media{Type: ct, Name: a.GetFileName(), Size: int64(a.GetSize()), Ref: ref}, true
}

func download(ctx context.Context, p *signalpb.AttachmentPointer) ([]byte, error) {
	return signalmeow.DownloadAttachmentWithPointer(ctx, p, nil, nil)
}

// Media downloads a stored message's file from Signal's CDN and decrypts it.
func (c *sgConnector) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	item, err := c.st.Media(ctx, message, index)
	if errors.Is(err, store.ErrNoMedia) {
		return connector.MediaFile{}, &connector.Error{
			Status:  http.StatusNotFound,
			Message: "no file to fetch: polls and contacts have none, and media is not available for messages stored before mandate could fetch it",
		}
	}
	if err != nil {
		return connector.MediaFile{}, err
	}
	var ref mediaRef
	if err := json.Unmarshal(item.Ref, &ref); err != nil {
		return connector.MediaFile{}, fmt.Errorf("media reference of %s: %w", message, err)
	}
	if _, _, connected := c.current(); !connected {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "signal is not connected"}
	}
	data, err := c.download(ctx, ref.pointer())
	switch {
	case errors.Is(err, signalmeow.ErrAttachmentNotFound):
		return connector.MediaFile{}, &connector.Error{Status: http.StatusNotFound, Message: "Signal's servers no longer have this file; they keep attachments only for a while"}
	case err != nil:
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Signal: " + err.Error()}
	}
	return connector.MediaFile{Type: item.Type, Name: item.Name, Size: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
}
