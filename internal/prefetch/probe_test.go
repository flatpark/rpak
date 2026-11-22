package prefetch

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"

	"github.com/flatpark/rpak/internal/metadata"
)

func TestProbe(t *testing.T) {
	srv := newServer(t)
	cfg := testConfig(t, t.TempDir())
	same := srv.add("/same.bin", payload(1000, 1))
	bigger := srv.add("/bigger.bin", payload(1000, 2))
	srv.files["/bigger.bin"] = payload(1200, 2)
	unknown := srv.add("/unknown.bin", payload(1000, 3))
	page := srv.add("/page.bin", payload(1000, 4))
	digest := srv.add("/digest.bin", payload(1000, 5))
	other := payload(1000, 6)
	gone := srv.add("/gone.bin", payload(1000, 7))
	gone.URI = srv.URL + "/missing.bin"
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		if r.Method != http.MethodHead {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/unknown.bin":
			// No length: chunked, as a PHP download script sends it.
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			return true
		case "/page.bin":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", "512")
			w.WriteHeader(http.StatusOK)
			return true
		case "/digest.bin":
			s := sumBytes(other)
			w.Header().Set("Repr-Digest", "sha-512=:AAAA:, sha-256=:"+base64.StdEncoding.EncodeToString(s)+":")
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			return true
		}
		return false
	}
	changes := New(cfg).Probe(context.Background(), []metadata.Source{same, bigger, unknown, page, digest, gone})
	got := map[string]Change{}
	for _, c := range changes {
		got[c.Source.Name] = c
	}
	if len(got) != 2 {
		t.Fatalf("changes %+v", changes)
	}
	if c := got["bigger.bin"]; c.Got.Size != 1200 || c.Got.Checksum != "" || c.Dir != "" {
		t.Errorf("bigger: %+v", c)
	}
	if c := got["digest.bin"]; c.Got.Size != 1000 || c.Got.Checksum != sum(other) {
		t.Errorf("digest: %+v", c)
	}
}

func sumBytes(b []byte) []byte {
	s, _ := hex.DecodeString(sum(b))
	return s
}

func TestHeaderSHA256(t *testing.T) {
	want := sum([]byte("x"))
	raw, _ := hex.DecodeString(want)
	b64 := base64.StdEncoding.EncodeToString(raw)
	for _, tc := range []struct {
		key, value, want string
	}{
		{"Repr-Digest", "sha-256=:" + b64 + ":", want},
		{"Digest", "MD5=abc, SHA-256=" + b64, want},
		{"X-Checksum-Sha256", want, want},
		{"X-Amz-Checksum-Sha256", b64, want},
		{"X-Amz-Checksum-Sha256", b64 + "-3", ""},
		{"Repr-Digest", "sha-256=:short:", ""},
	} {
		h := http.Header{}
		h.Set(tc.key, tc.value)
		if got := headerSHA256(h); got != tc.want {
			t.Errorf("%s: %s = %q, want %q", tc.key, tc.value, got, tc.want)
		}
	}
}
