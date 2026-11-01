package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMediaSavesFileWithExtension(t *testing.T) {
	t.Chdir(t.TempDir())
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.URL.Query().Get("index") == "1" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="../../Plan.PDF"`)
			w.Write([]byte("%PDF"))
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg!"))
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		args       []string
		query      string
		path, body string
		out        string
	}{
		{
			[]string{"123@g.us", "7"},
			"chat=123%40g.us&seq=7",
			"whatsapp-7.jpg", "jpeg!", "path\ttype\tsize\tname\nwhatsapp-7.jpg\timage/jpeg\t5\t\n",
		},
		{
			[]string{"123@g.us", "7", "--index", "1", "--json"},
			"chat=123%40g.us&index=1&seq=7",
			"whatsapp-7-1.pdf", "%PDF", `{"path":"whatsapp-7-1.pdf","type":"application/octet-stream","size":4,"name":"../../Plan.PDF"}` + "\n",
		},
		{
			[]string{"123@g.us", "7", "-o", "cat"},
			"chat=123%40g.us&seq=7",
			"cat", "jpeg!", "path\ttype\tsize\tname\ncat\timage/jpeg\t5\t\n",
		},
	} {
		c, out, _ := testClient(t, srv.URL, false)
		if err := c.connector(t.Context(), "whatsapp", append([]string{"media"}, tc.args...)); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if query != tc.query {
			t.Errorf("%v: query %q, want %q", tc.args, query, tc.query)
		}
		if b, err := os.ReadFile(tc.path); err != nil || string(b) != tc.body {
			t.Errorf("%v: %s = %q %v, want %q", tc.args, tc.path, b, err, tc.body)
		}
		if out.String() != tc.out {
			t.Errorf("%v: printed %q, want %q", tc.args, out, tc.out)
		}
	}
}
