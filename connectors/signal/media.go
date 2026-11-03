package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"

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

// knownCDN reports whether signalmeow has a host for CDN n; it indexes its
// host list with n, so a larger n from a sender would panic.
func knownCDN(n uint32) bool { return int(n) < len(web.CDNHosts) }

func attachment(a *signalpb.AttachmentPointer) (store.Media, bool) {
	if len(a.GetKey()) != 64 || len(a.GetDigest()) == 0 || (a.GetCdnId() == 0 && a.GetCdnKey() == "") || !knownCDN(a.GetCdnNumber()) {
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

func download(ctx context.Context, p *signalpb.AttachmentPointer, into *os.File) error {
	_, err := signalmeow.DownloadAttachmentWithPointer(ctx, p, nil, into)
	return err
}

// Media downloads a stored message's file from Signal's CDN and decrypts it
// in an unlinked temporary file.
func (c *sgConnector) Media(ctx context.Context, message string, index int, limit int64) (connector.MediaFile, error) {
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
	if !knownCDN(ref.CdnNumber) {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "the file is on a Signal CDN mandate does not know"}
	}
	if _, _, connected := c.current(); !connected {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "signal is not connected"}
	}
	f, err := os.CreateTemp("", "media-")
	if err != nil {
		return connector.MediaFile{}, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return connector.MediaFile{}, err
	}
	err = c.download(context.WithValue(ctx, mediaCapKey{}, encryptedMax(limit)), ref.pointer(), f)
	var size int64
	if err == nil {
		var fi os.FileInfo
		if fi, err = f.Stat(); err == nil {
			size = fi.Size()
			_, err = f.Seek(0, io.SeekStart)
		}
	}
	if err == nil && size <= limit {
		return connector.MediaFile{Type: item.Type, Name: item.Name, Size: size, Body: f}, nil
	}
	f.Close()
	switch {
	case err == nil, errors.Is(err, errOverLimit):
		return connector.MediaFile{}, connector.TooLarge(limit)
	case errors.Is(err, signalmeow.ErrAttachmentNotFound):
		return connector.MediaFile{}, &connector.Error{Status: http.StatusNotFound, Message: "Signal's servers no longer have this file; they keep attachments only for a while"}
	}
	return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Signal: " + err.Error()}
}

// encryptedMax is the largest download that can hold a file of limit
// bytes: Signal pads files by up to 5%, to at least 541 bytes, and adds an
// IV, cipher padding and a MAC.
func encryptedMax(limit int64) int64 { return limit + limit/16 + 1024 }

type mediaCapKey struct{}

var errOverLimit = errors.New("file over the media limit")

// capMedia stops an attachment download whose context carries a cap past
// it: signalmeow allocates whatever size the CDN states and reads the body
// without a bound.
type capMedia struct{ http.RoundTripper }

func (t capMedia) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	limit, ok := req.Context().Value(mediaCapKey{}).(int64)
	if err != nil || !ok {
		return resp, err
	}
	if resp.ContentLength > limit {
		resp.Body.Close()
		return nil, errOverLimit
	}
	resp.Body = &cappedBody{ReadCloser: resp.Body, left: limit}
	return resp, nil
}

type cappedBody struct {
	io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if int64(n) > b.left {
		return 0, errOverLimit
	}
	b.left -= int64(n)
	return n, err
}
