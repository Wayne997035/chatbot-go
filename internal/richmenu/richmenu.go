// Package richmenu 建立聊天室下方的圖片選單（Rich Menu）.
package richmenu

import (
	"bytes"
	"chatbot-go/internal/redact"
	"context"
	_ "embed" // 選單圖片.
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

//go:embed richmenu.png
var image []byte

// 選單名稱；改版時遞增版本號，舊版會在新版設好後刪除.
const (
	namePrefix = "chatbot-go-main"
	name       = namePrefix + "-v1"
	width      = 2500
	height     = 1686
)

// buttons 由左到右、由上到下，點了會替使用者送出對應文字.
var buttons = []string{"查天氣", "拍食物", "記運動", "今日統計", "本週統計", "說明"}

// Buttons 回傳選單每一格點擊後送出的文字（依位置順序）.
func Buttons() []string {
	return append([]string(nil), buttons...)
}

// API 位址.
const (
	DefaultAPIBase  = "https://api.line.me"
	DefaultDataBase = "https://api-data.line.me"
)

// Definition 回傳 LINE Rich Menu 定義.
func Definition() map[string]any {
	const cols, rows = 3, 2
	cellW, cellH := width/cols, height/rows
	areas := make([]map[string]any, 0, len(buttons))
	for i, text := range buttons {
		areas = append(areas, map[string]any{
			"bounds": map[string]int{"x": (i % cols) * cellW, "y": (i / cols) * cellH, "width": cellW, "height": cellH},
			"action": map[string]string{"type": "message", "label": text, "text": text},
		})
	}
	return map[string]any{
		"size":        map[string]int{"width": width, "height": height},
		"selected":    true,
		"name":        name,
		"chatBarText": "選單",
		"areas":       areas,
	}
}

// Client 呼叫 LINE Rich Menu API.
type Client struct {
	HTTP     *http.Client
	Token    string
	APIBase  string
	DataBase string
}

// Setup 建立新選單、上傳圖片、設為所有人的預設選單，最後刪除舊版本. 回傳新選單 ID.
func (c *Client) Setup(ctx context.Context) (string, error) {
	var created struct {
		RichMenuID string `json:"richMenuId"`
	}
	body, err := json.Marshal(Definition())
	if err != nil {
		return "", fmt.Errorf("encode rich menu: %w", err)
	}
	if err := c.do(ctx, http.MethodPost, c.APIBase+"/v2/bot/richmenu", "application/json", body, &created); err != nil {
		return "", fmt.Errorf("create rich menu: %w", err)
	}
	id := created.RichMenuID
	if err := c.do(ctx, http.MethodPost, c.DataBase+"/v2/bot/richmenu/"+id+"/content", "image/png", image, nil); err != nil {
		return "", fmt.Errorf("upload rich menu image: %w", err)
	}
	if err := c.do(ctx, http.MethodPost, c.APIBase+"/v2/bot/user/all/richmenu/"+id, "", nil, nil); err != nil {
		return "", fmt.Errorf("set default rich menu: %w", err)
	}
	if err := c.deleteOld(ctx, id); err != nil {
		return id, fmt.Errorf("new menu is live, but removing old versions failed: %w", err)
	}
	return id, nil
}

func (c *Client) deleteOld(ctx context.Context, keepID string) error {
	var list struct {
		RichMenus []struct {
			RichMenuID string `json:"richMenuId"`
			Name       string `json:"name"`
		} `json:"richmenus"`
	}
	if err := c.do(ctx, http.MethodGet, c.APIBase+"/v2/bot/richmenu/list", "", nil, &list); err != nil {
		return err
	}
	for _, m := range list.RichMenus {
		if m.RichMenuID != keepID && strings.HasPrefix(m.Name, namePrefix) {
			if err := c.do(ctx, http.MethodDelete, c.APIBase+"/v2/bot/richmenu/"+m.RichMenuID, "", nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, url, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("LINE API returned %d: %s", resp.StatusCode, redact.String(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
