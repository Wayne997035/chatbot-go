package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// imageCaptureProvider [F37-34] 記下收到的第一張圖的位元組與呼叫次數.
type imageCaptureProvider struct {
	name  string
	calls int
	got   []byte
}

func (p *imageCaptureProvider) Name() string { return p.name }

func (*imageCaptureProvider) Candidates(context.Context, Capability) ([]string, error) {
	return []string{"vision-model"}, nil
}

func (p *imageCaptureProvider) Call(_ context.Context, _ string, req *Request) (string, error) {
	p.calls++
	if len(req.Images) > 0 {
		p.got = append([]byte{}, req.Images[0].Data...)
	}
	return `{"ok":true}`, nil
}

// imageGPSSentinel 夾在假 Exif 裡的哨兵.
const imageGPSSentinel = "GPSSENTINEL"

// imageExifJPEG 一張真的可解碼 JPEG，SOI 之後插入帶哨兵的 Exif APP1，尾端接一個帶哨兵的副圖.
func imageExifJPEG(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	base := buf.Bytes()
	payload := append([]byte("Exif\x00\x00MM\x00\x2A\x00\x00\x00\x08\x00\x00\x00\x00\x00\x00"), imageGPSSentinel...)
	n := len(payload) + 2
	if n > math.MaxUint16 {
		t.Fatalf("fixture APP1 too large: %d", n)
		return nil
	}
	seg := binary.BigEndian.AppendUint16([]byte{0xFF, 0xE1}, uint16(n))
	seg = append(seg, payload...)
	out := append(append(append([]byte{}, base[:2]...), seg...), base[2:]...)
	return append(out, 0xFF, 0xD8, 0xFF, 0xD9, 'T', 'R', 'A', 'I', 'L')
}

func imageVisionRouter(p Provider) *Router {
	return NewRouter(&Settings{VisionOrder: []string{p.Name()}, TextOrder: []string{p.Name()}}, p)
}

// [F37-34] 供應商收到的照片沒有哨兵；呼叫端的原始位元組沒被改到.
func TestCompleteSanitizesImages(t *testing.T) {
	prov := &imageCaptureProvider{name: "groq"}
	in := imageExifJPEG(t)
	orig := append([]byte{}, in...)
	res := imageVisionRouter(prov).Complete(context.Background(), &Request{Prompt: "p", Images: []Image{{MIME: "image/jpeg", Data: in}}})
	if !res.OK() || res.ImageRejected {
		t.Fatalf("res = %+v", res)
	}
	if bytes.Contains(prov.got, []byte(imageGPSSentinel)) || bytes.Contains(prov.got, []byte("TRAIL")) {
		t.Errorf("provider received metadata: %q", prov.got)
	}
	if !bytes.Equal(in, orig) || !bytes.Contains(in, []byte(imageGPSSentinel)) {
		t.Errorf("caller's bytes were mutated")
	}
}

// [F37-34] 壞掉的 JPEG：不呼叫任何供應商、OK 為 false、Attempts 0、ImageRejected、額度完全沒扣.
func TestCompleteRejectsUnsanitizableImage(t *testing.T) {
	damaged := imageExifJPEG(t)[:30] // 截斷：沒有 EOI
	prov := &imageCaptureProvider{name: "groq"}
	r := imageVisionRouter(prov)
	ctx := EnsureBudget(context.Background())

	for range MessageCalls + 2 {
		if !CanSpend(ctx, false) {
			t.Fatal("budget was consumed by a rejected photo")
		}
		res := r.Complete(ctx, &Request{Prompt: "p", Images: []Image{{MIME: "image/jpeg", Data: damaged}}})
		if res.OK() || res.Attempts != 0 || !res.ImageRejected || res.Skipped || len(res.Errors) == 0 {
			t.Fatalf("res = %+v, want rejected without attempts, skip or budget use", res)
		}
	}
	if prov.calls != 0 {
		t.Errorf("provider called %d times for a damaged photo (fail-open)", prov.calls)
	}
	if b := budgetOf(ctx); b.used != 0 || b.usedPr != 0 {
		t.Errorf("budget used = %d/%d, want 0/0", b.used, b.usedPr)
	}

	// 正對照：同一個 Router 與 ctx，文字請求與正常照片都正常、ImageRejected 為 false.
	text := r.Complete(ctx, &Request{Prompt: "p"})
	photo := r.Complete(ctx, &Request{Prompt: "p", Images: []Image{{MIME: "image/jpeg", Data: imageExifJPEG(t)}}})
	if !text.OK() || text.ImageRejected || !photo.OK() || photo.ImageRejected || prov.calls != 2 {
		t.Errorf("text = %+v, photo = %+v, provider calls = %d", text, photo, prov.calls)
	}
}

var (
	imageDataURLRe = regexp.MustCompile(`base64,([A-Za-z0-9+/=]+)`)
	imageInlineRe  = regexp.MustCompile(`"data":"([A-Za-z0-9+/=]+)"`)
)

// imageSentBytes 從供應商收到的 HTTP body 取出照片位元組.
func imageSentBytes(t *testing.T, body map[string]any, re *regexp.Regexp) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no image in body: %s", raw)
	}
	data, err := base64.StdEncoding.DecodeString(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// [F37-34] OpenAI 相容（Groq）與 Gemini 的 HTTP body 經 Router 建構後都不含哨兵.
func TestProvidersNeverSendImageMetadata(t *testing.T) {
	in := imageExifJPEG(t)
	req := func() *Request {
		return &Request{Prompt: "看圖", Images: []Image{{MIME: "image/jpeg", Data: in}}}
	}

	t.Run("openai_compatible", func(t *testing.T) {
		srv, c := newServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"data":[{"id":"meta-llama/llama-4-scout-17b-16e-instruct"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"a\":1}"}}]}`)
		})
		p := NewGroq("k", srv.Client()).(*OpenAICompatible)
		p.baseURL = srv.URL
		if res := imageVisionRouter(p).Complete(context.Background(), req()); !res.OK() {
			t.Fatalf("res = %+v", res)
		}
		sent := imageSentBytes(t, c.body, imageDataURLRe)
		if bytes.Contains(sent, []byte(imageGPSSentinel)) || bytes.Contains(sent, []byte("TRAIL")) {
			t.Errorf("OpenAI-compatible body carries metadata")
		}
	})

	t.Run("gemini", func(t *testing.T) {
		srv, c := newServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
			if r.Method == http.MethodGet {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"{\"a\":1}"}]}}]}`)
		})
		g := NewGemini("k", srv.Client()).(*Gemini)
		g.baseURL = srv.URL
		if res := imageVisionRouter(g).Complete(context.Background(), req()); !res.OK() {
			t.Fatalf("res = %+v", res)
		}
		sent := imageSentBytes(t, c.body, imageInlineRe)
		if bytes.Contains(sent, []byte(imageGPSSentinel)) || bytes.Contains(sent, []byte("TRAIL")) {
			t.Errorf("Gemini body carries metadata")
		}
	})

	// 正對照：直接呼叫供應商（略過 Router）時哨兵確實會出現，證明上面的檢查看得到它.
	t.Run("control_bypassing_router_leaks", func(t *testing.T) {
		srv, c := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
		})
		p := NewGroq("k", srv.Client()).(*OpenAICompatible)
		p.baseURL = srv.URL
		if _, err := p.Call(context.Background(), "m", req()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(imageSentBytes(t, c.body, imageDataURLRe)), imageGPSSentinel) {
			t.Errorf("control failed: sentinel not visible when bypassing Router")
		}
	})
}
