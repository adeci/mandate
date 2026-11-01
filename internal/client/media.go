package client

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type savedMedia struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Name string `json:"name,omitempty"`
}

// media saves a message's file and prints where it went.
func (c *client) media(ctx context.Context, name string, args []string) error {
	help := fmt.Sprintf(mediaUsage, name)
	fs := flag.NewFlagSet("media", flag.ContinueOnError)
	index := fs.Int("index", 0, "")
	out := fs.String("o", "", "")
	pos, err := c.parse(fs, help, args, 2, 2)
	if err != nil {
		return err
	}
	seq, err := strconv.Atoi(pos[1])
	if err != nil || seq < 1 {
		return usage(help, fmt.Errorf("SEQ is a message's seq from mandate %s messages, not %q", name, pos[1]))
	}
	if *index < 0 {
		return usage(help, errors.New("--index must not be negative"))
	}
	ctx, cancel := context.WithTimeout(ctx, mediaTimeout)
	defer cancel()
	resp, err := c.send(ctx, http.MethodGet, "/v1/"+url.PathEscape(name)+"/media"+query("chat", pos[0], "seq", seq, "index", *index), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	saved := savedMedia{Path: *out, Type: resp.Header.Get("Content-Type")}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		saved.Name = params["filename"]
	}
	if saved.Path == "" {
		saved.Path = mediaName(name, seq, *index, saved.Type, saved.Name)
	}
	f, err := os.Create(saved.Path)
	if err != nil {
		return err
	}
	saved.Size, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(saved.Path)
		return fmt.Errorf("save %s: %w", saved.Path, err)
	}
	if c.json {
		return json.NewEncoder(c.out).Encode(saved)
	}
	raw, err := json.Marshal(table{Columns: []string{"path", "type", "size", "name"}, Rows: [][]any{{saved.Path, saved.Type, saved.Size, saved.Name}}})
	if err != nil {
		return err
	}
	return c.table(raw)
}

// mediaName names a file after the connector, message and index, so fetching
// it again replaces it. Of the sender's file name only the extension is used.
func mediaName(connector string, seq, index int, typ, name string) string {
	base := fmt.Sprintf("%s-%d", connector, seq)
	if index > 0 {
		base += fmt.Sprintf("-%d", index)
	}
	return base + extension(typ, name)
}

var plainExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

// Extensions for the types chats carry most, where mime's answer depends on
// the system's mime.types or lists several.
var extensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"video/webm":      ".webm",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"audio/aac":       ".aac",
	"audio/amr":       ".amr",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

func extension(typ, name string) string {
	if ext := filepath.Ext(name); plainExt.MatchString(ext) {
		return strings.ToLower(ext)
	}
	mt, _, err := mime.ParseMediaType(typ)
	if err != nil {
		return ""
	}
	if ext, ok := extensions[mt]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ""
}
