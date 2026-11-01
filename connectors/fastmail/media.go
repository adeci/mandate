package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// maxMedia is the most fetched into memory when Fastmail does not say how
// large a file is; the gateway refuses larger files anyway.
const maxMedia = 100 << 20

// media is the files of parts; Ref is the blob ID.
func media(parts []bodyPart) []store.Media {
	files := make([]store.Media, 0, len(parts))
	for _, p := range parts {
		files = append(files, store.Media{
			Type: cmp.Or(p.Type, "application/octet-stream"), Name: p.Name, Size: p.Size, Ref: []byte(p.BlobID),
		})
	}
	return files
}

var errNoFile = &connector.Error{Status: http.StatusNotFound, Message: "the email has no such attachment, or it is no longer on Fastmail"}

func (c *fmConnector) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	a, cn, err := c.linked()
	if err != nil {
		return connector.MediaFile{}, err
	}
	item, err := c.st.Media(ctx, message, index)
	if errors.Is(err, store.ErrNoMedia) {
		return connector.MediaFile{}, errNoFile
	} else if err != nil {
		return connector.MediaFile{}, err
	}
	if cn.download == "" {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "the Fastmail session names no download URL"}
	}
	u := strings.NewReplacer(
		"{accountId}", url.PathEscape(cn.accountID),
		"{blobId}", url.PathEscape(string(item.Ref)),
		"{type}", url.PathEscape(item.Type),
		"{name}", url.PathEscape(cmp.Or(item.Name, "file")),
	).Replace(cn.download)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return connector.MediaFile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Fastmail: " + err.Error()}
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		resp.Body.Close()
		return connector.MediaFile{}, errNoFile
	default:
		resp.Body.Close()
		return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Fastmail: " + resp.Status}
	}
	file := connector.MediaFile{Type: item.Type, Name: item.Name, Size: resp.ContentLength, Body: resp.Body}
	if file.Size < 0 {
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxMedia+1))
		if err != nil {
			return connector.MediaFile{}, &connector.Error{Status: http.StatusBadGateway, Message: "download from Fastmail: " + err.Error()}
		}
		file.Size, file.Body = int64(len(data)), io.NopCloser(bytes.NewReader(data))
	}
	return file, nil
}
