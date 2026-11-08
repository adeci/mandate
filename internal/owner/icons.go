package owner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // decodes the pictures connectors send
	"image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
)

const (
	// iconSize is the side of the square PNG the app gets.
	iconSize = 64
	// maxIconSide bounds a picture's sides before it is decoded.
	maxIconSide = 2048
	// iconCache bounds the cached pictures; the least recently served go first.
	iconCache = 20 << 20
)

// icons is the owner side's cache of chats' pictures, as PNGs it made, in
// its state directory: icons/CONNECTOR/HASH-VERSION.png.
type icons struct {
	mu sync.Mutex // guards pruning
}

func iconFile(dir, conn, chat, version string) string {
	h := sha256.Sum256([]byte(chat))
	return filepath.Join(dir, "icons", conn, hex.EncodeToString(h[:16])+"-"+version+".png")
}

func isIconVersion(v string) bool {
	if v == "" || len(v) > 32 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// connectorIcon serves a chat's picture at the version the listing gave,
// as a 64 px PNG made here, so the app decodes only pictures this server
// wrote. A versioned picture never changes, so the app may keep it.
func (s *Server) connectorIcon(w http.ResponseWriter, r *http.Request) {
	c := s.conn(w, r)
	if c == nil {
		return
	}
	conn, chat, version := r.PathValue("name"), r.URL.Query().Get("chat"), r.URL.Query().Get("v")
	if chat == "" || !isIconVersion(version) {
		fail(w, http.StatusBadRequest, "chat and v, the icon version of the chat's listing, are required")
		return
	}
	path := iconFile(s.dir, conn, chat, version)
	if b, err := os.ReadFile(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now) // recently served
		servePNG(w, b, true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ic, got, err := c.Icon(ctx, chat)
	if err != nil {
		if e, ok := errors.AsType[*connector.Error](err); ok && e.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(max(e.RetryAfter, 1)))
			fail(w, http.StatusServiceUnavailable, "the connector is fetching other pictures; try again")
			return
		}
		failErr(w, connectorError(err))
		return
	}
	b, err := iconPNG(ic.Body)
	if err != nil {
		s.log.Warn("chat picture", "connector", conn, "err", err)
		fail(w, http.StatusBadGateway, "the connector's picture is not a usable image")
		return
	}
	if isIconVersion(got) {
		if err := s.icons.keep(s.dir, iconFile(s.dir, conn, chat, got), b); err != nil {
			s.log.Warn("cache chat picture", "err", err)
		}
	}
	// The picture changed since the listing: right now, but not for keeps.
	servePNG(w, b, got == version)
}

func servePNG(w http.ResponseWriter, b []byte, versioned bool) {
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-store")
	if versioned {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	_, _ = w.Write(b)
}

// iconPNG decodes a PNG or JPEG of sane size, crops it to its centred
// square and writes that as an iconSize PNG.
func iconPNG(b []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if format != "png" && format != "jpeg" {
		return nil, fmt.Errorf("a %s picture", format)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxIconSide || cfg.Height > maxIconSide {
		return nil, fmt.Errorf("a picture of %d×%d", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, square(src, iconSize)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// square scales the centred square of src to size×size, each pixel the
// average of the source pixels it covers.
func square(src image.Image, size int) *image.NRGBA {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	x0, y0 := b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		sy0, sy1 := y0+y*side/size, y0+max((y+1)*side/size, y*side/size+1)
		for x := range size {
			sx0, sx1 := x0+x*side/size, x0+max((x+1)*side/size, x*side/size+1)
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					c := color.NRGBA64Model.Convert(src.At(sx, sy)).(color.NRGBA64)
					r, g, bl, a, n = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			dst.SetNRGBA(x, y, color.NRGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return dst
}

// keep writes a picture to the cache and drops the least recently served
// past iconCache.
func (ic *icons) keep(dir, path string, b []byte) error {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := writeFile(path, b, 0o600); err != nil {
		return err
	}
	type cached struct {
		path string
		size int64
		at   time.Time
	}
	var all []cached
	var total int64
	err := filepath.WalkDir(filepath.Join(dir, "icons"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		all = append(all, cached{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(all, func(a, b cached) int { return a.at.Compare(b.at) })
	for _, c := range all {
		if total <= iconCache {
			break
		}
		if c.path == path {
			continue
		}
		if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		total -= c.size
	}
	return nil
}
