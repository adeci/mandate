package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

const (
	defaultCDNBase = "https://cdn.discordapp.com"
	cdnTemplate    = "/attachments/{channel_id}/{attachment_id}/{filename}"
	refreshPath    = "/attachments/refresh-urls"
	jumpPage       = 50 // messages the web client loads around one it jumps to
)

// Attachment links are signed and expire within a day, so only the path is
// kept. A forward's files are looked up again through the forwarding
// message, which is the one stored.
type attachmentRef struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	ID        string `json:"id"`
	Path      string `json:"path,omitempty"`
}

var cdnPathRe = regexp.MustCompile(`^/attachments/(?:\d{15,21}/){2,3}[^/?#]+$`)

// cdnPath is link's attachment path, "" if it is none.
func cdnPath(link string) string {
	u, err := url.Parse(link)
	if err != nil || !cdnPathRe.MatchString(u.EscapedPath()) {
		return ""
	}
	return u.EscapedPath()
}

func storedFiles(channelID, messageID string, as []apiAttachment) []store.Media {
	if as == nil {
		return nil // unknown, so the stored files stay
	}
	out := make([]store.Media, len(as))
	for i, a := range as {
		ref, err := json.Marshal(attachmentRef{ChannelID: channelID, MessageID: messageID, ID: a.ID, Path: cdnPath(a.URL)})
		if err != nil {
			panic(err) // strings
		}
		out[i] = store.Media{Type: fileType(a.ContentType), Name: a.Filename, Size: a.Size, Ref: ref}
	}
	return out
}

func fileType(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}

func notFound(msg string) error {
	return &connector.Error{Status: http.StatusNotFound, Message: msg}
}

// Media runs on the worker like a read; only the download's body is read
// after the call, under ctx.
func (w *worker) Media(ctx context.Context, message string, index int) (connector.MediaFile, error) {
	var f connector.MediaFile
	_, err := w.submit(ctx, "media", false, func(jctx context.Context, dl time.Time) (connector.ReadResult, error) {
		var err error
		f, err = w.svc.media(jctx, ctx, dl, message, index)
		return connector.ReadResult{}, err
	})
	return f, err
}

// media refreshes the stored link of a file and starts its download. A
// message stored before its files were is looked up again on Discord.
func (s *service) media(ctx, bodyCtx context.Context, dl time.Time, message string, index int) (connector.MediaFile, error) {
	item, err := s.st.Media(ctx, message, index)
	var ref attachmentRef
	switch {
	case errors.Is(err, store.ErrNoMedia):
		var kind string
		err := s.st.DB().QueryRowContext(ctx, `SELECT container_id, COALESCE(media_type, '') FROM messages WHERE id = ?1 AND NOT deleted`, message).
			Scan(&ref.ChannelID, &kind)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return connector.MediaFile{}, notFound("no such message")
		case err != nil:
			return connector.MediaFile{}, err
		case kind == "sticker" || kind == mediaEmbed:
			return connector.MediaFile{}, notFound("stickers and embeds have no file to fetch")
		}
		ref.MessageID = message
	case err != nil:
		return connector.MediaFile{}, err
	default:
		if err := json.Unmarshal(item.Ref, &ref); err != nil {
			return connector.MediaFile{}, fmt.Errorf("file reference of %s: %w", message, err)
		}
	}
	if !isSnowflake(ref.ChannelID) || !isSnowflake(ref.MessageID) {
		return connector.MediaFile{}, notFound("no file stored for this message")
	}
	c, err := s.storedContainer(ctx, ref.ChannelID)
	if err != nil {
		return connector.MediaFile{}, err
	}
	guild, err := s.guildOf(ctx, c)
	if err != nil {
		return connector.MediaFile{}, err
	}
	referer := channelURL(guild, ref.ChannelID)
	var link string
	if ref.Path != "" {
		link, err = s.refresh(ctx, dl, referer, ref.Path)
	} else {
		var a apiAttachment
		a, err = s.refetch(ctx, dl, referer, ref, index)
		item = store.Media{Type: fileType(a.ContentType), Name: a.Filename, Size: a.Size}
		link = a.URL
	}
	if err != nil {
		return connector.MediaFile{}, err
	}
	body, size, err := s.up.download(ctx, bodyCtx, dl, link, item.Size)
	if err != nil {
		return connector.MediaFile{}, err
	}
	return connector.MediaFile{Type: item.Type, Name: item.Name, Size: size, Body: body}, nil
}

// refresh signs an attachment path anew, as the web client does for links
// that expired while it had them.
func (s *service) refresh(ctx context.Context, dl time.Time, referer, path string) (string, error) {
	if err := s.up.ensureBuildNumber(ctx, dl); err != nil {
		return "", err
	}
	r := route{template: refreshPath, path: refreshPath, referer: referer}
	body := func() []byte {
		b, err := json.Marshal(map[string][]string{"attachment_urls": {s.up.cdnBase + path}})
		if err != nil {
			panic(err) // strings
		}
		return b
	}
	var res struct {
		RefreshedURLs []struct {
			Refreshed string `json:"refreshed"`
		} `json:"refreshed_urls"`
	}
	if _, err := s.up.post(ctx, dl, kindAPI, r, time.Time{}, body, &res); err != nil {
		var refused *refusedError
		if errors.As(err, &refused) {
			return "", &upstreamError{status: refused.status, msg: refused.msg}
		}
		return "", err
	}
	if len(res.RefreshedURLs) == 0 {
		return "", &upstreamError{status: http.StatusOK, msg: ": " + refreshPath + " without a link"}
	}
	return res.RefreshedURLs[0].Refreshed, nil
}

// refetch loads the messages around ref's, as jumping to it does, and
// returns its file: the one with ref's ID, else the index-th.
func (s *service) refetch(ctx context.Context, dl time.Time, referer string, ref attachmentRef, index int) (apiAttachment, error) {
	r := route{
		template: "/channels/{channel_id}/messages",
		path:     "/channels/" + ref.ChannelID + "/messages",
		query:    url.Values{"around": {ref.MessageID}, "limit": {strconv.Itoa(jumpPage)}},
		major:    ref.ChannelID,
		referer:  referer,
		deny:     "channel:" + ref.ChannelID,
		denyWhat: "channel " + ref.ChannelID,
	}
	var msgs []apiMessage
	if err := s.up.get(ctx, dl, r, &msgs); err != nil {
		return apiAttachment{}, err
	}
	for i := range msgs {
		if msgs[i].ID != ref.MessageID {
			continue
		}
		as := carried(&msgs[i]).Attachments
		if ref.ID != "" {
			for _, a := range as {
				if a.ID == ref.ID {
					return a, nil
				}
			}
			return apiAttachment{}, notFound("the file was removed from the message on Discord")
		}
		if index >= len(as) {
			return apiAttachment{}, notFound(fmt.Sprintf("the message has %d files on Discord", len(as)))
		}
		return as[index], nil
	}
	return apiAttachment{}, notFound("the message is gone from Discord")
}

// download opens a signed attachment link like a click on the file in the
// app. The request waits for its slot under ctx; its body is read under
// bodyCtx once the call is over.
func (u *upstream) download(ctx, bodyCtx context.Context, dl time.Time, link string, known int64) (io.ReadCloser, int64, error) {
	if u.fatal != nil {
		return nil, 0, u.fatal
	}
	l, err := url.Parse(link)
	if err != nil || !cdnPathRe.MatchString(l.EscapedPath()) {
		return nil, 0, &upstreamError{status: http.StatusOK, msg: ": no attachment link"}
	}
	target := u.cdnBase + l.EscapedPath() + "?" + l.RawQuery
	r := route{template: cdnTemplate}
	wait, reason, err := u.await(ctx, dl, kindFollow, r, time.Time{})
	if err != nil {
		return nil, 0, err
	}
	rctx, cancel := context.WithCancel(bodyCtx)
	req, err := fhttp.NewRequestWithContext(rctx, fhttp.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	applyHeaders(req, attachmentHeaders())
	// Only the response must come within requestTimeout; a large file may
	// take longer to arrive.
	timer := time.AfterFunc(requestTimeout, cancel)
	sent := u.clock.Now()
	resp, err := u.cdn.Do(req)
	timer.Stop()
	u.pace.record(sent)
	u.log.Info("upstream", "method", fhttp.MethodGet, "route", cdnTemplate, "status", statusOf(resp),
		"wait", wait.Round(time.Millisecond), "wait_reason", reason,
		"duration", u.clock.Now().Sub(sent).Round(time.Millisecond),
		"budget_used", len(u.st.Requests))
	if serr := u.saveState(); serr != nil {
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		return nil, 0, serr
	}
	if err != nil {
		cancel()
		if bodyCtx.Err() != nil {
			return nil, 0, bodyCtx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, &upstreamError{msg: cdnTemplate + ": " + err.Error()}
	}
	switch resp.StatusCode {
	case fhttp.StatusOK:
	case fhttp.StatusForbidden, fhttp.StatusNotFound, fhttp.StatusGone:
		resp.Body.Close()
		cancel()
		return nil, 0, notFound("the file is gone from Discord")
	default:
		resp.Body.Close()
		cancel()
		return nil, 0, &upstreamError{status: resp.StatusCode, msg: " for the file"}
	}
	// A body fhttp decompressed has no length; Discord's size is the file's.
	size := resp.ContentLength
	if size < 0 {
		size = known
	}
	if size <= 0 {
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			return nil, 0, &upstreamError{msg: cdnTemplate + ": " + err.Error()}
		}
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}
	return &fileBody{Reader: io.LimitReader(resp.Body, size), body: resp.Body, cancel: cancel}, size, nil
}

type fileBody struct {
	io.Reader
	body   io.Closer
	cancel context.CancelFunc
}

func (f *fileBody) Close() error {
	err := f.body.Close()
	f.cancel()
	return err
}
