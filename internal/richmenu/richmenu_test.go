package richmenu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefinitionCoversWholeImage(t *testing.T) {
	def := Definition("")
	areas := def["areas"].([]map[string]any)
	if len(areas) != len(buttons) {
		t.Fatalf("areas = %d", len(areas))
	}
	var covered int
	for _, a := range areas {
		b := a["bounds"].(map[string]int)
		covered += b["width"] * b["height"]
	}
	if covered < width*height-width*2 {
		t.Errorf("areas cover %d of %d pixels", covered, width*height)
	}
	if len(image) == 0 || len(image) > 1<<20 {
		t.Errorf("image size %d must be within LINE's 1MB limit", len(image))
	}
}

func TestSetupCreatesUploadsSetsDefaultAndRemovesOldVersions(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header missing")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/v2/bot/richmenu" && r.Method == http.MethodPost:
			var def map[string]any
			_ = json.Unmarshal(body, &def)
			if def["name"] != name {
				t.Errorf("name = %v", def["name"])
			}
			_, _ = io.WriteString(w, `{"richMenuId":"new"}`)
		case strings.HasSuffix(r.URL.Path, "/content"):
			if r.Header.Get("Content-Type") != "image/png" || len(body) != len(image) {
				t.Errorf("image upload: %s %d bytes", r.Header.Get("Content-Type"), len(body))
			}
		case r.URL.Path == "/v2/bot/richmenu/list":
			_, _ = io.WriteString(w, `{"richmenus":[{"richMenuId":"new","name":"chatbot-go-main-v2"},
				{"richMenuId":"old","name":"chatbot-go-main-v1"},{"richMenuId":"other","name":"campaign"}]}`)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), Token: "tok", APIBase: srv.URL, DataBase: srv.URL}
	id, err := c.Setup(context.Background())
	if err != nil || id != "new" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	want := "POST /v2/bot/richmenu,POST /v2/bot/richmenu/new/content,POST /v2/bot/user/all/richmenu/new," +
		"GET /v2/bot/richmenu/list,DELETE /v2/bot/richmenu/old"
	if got := strings.Join(calls, ","); got != want {
		t.Errorf("calls:\n got %s\nwant %s", got, want)
	}
}

func TestSetupReportsLINEErrorsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Authentication failed. token=abcdef"}`)
	}))
	defer srv.Close()
	_, err := (&Client{HTTP: srv.Client(), Token: "tok", APIBase: srv.URL, DataBase: srv.URL}).Setup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "abcdef") {
		t.Errorf("err = %v", err)
	}
}

func TestStatsTileOpensLIFFWhenConfigured(t *testing.T) {
	areas := Definition("https://liff.line.me/123-abc")["areas"].([]map[string]any)
	stats := areas[3]["action"].(map[string]string)
	if stats["type"] != "uri" || stats["uri"] != "https://liff.line.me/123-abc" || stats["label"] != "我的統計" {
		t.Errorf("stats tile = %v", stats)
	}
	ask := areas[4]["action"].(map[string]string)
	if ask["type"] != "message" || ask["text"] != "問 AI" {
		t.Errorf("ask tile = %v", ask)
	}
	fallback := Definition("")["areas"].([]map[string]any)[3]["action"].(map[string]string)
	if fallback["type"] != "message" || fallback["text"] != "今日統計" {
		t.Errorf("without LIFF the stats tile should send 今日統計, got %v", fallback)
	}
}
