package liff

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/subscription"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// maxAlertBodyBytes R7 body 上限：400 項地區（每項最長約 40 byte）加 JSON 結構遠小於此；超過直接 400，不讀完.
const maxAlertBodyBytes = 64 << 10

// AlertItem [F30-05] 一項開啟中的通知；regions 永遠是陣列（地震海嘯為 []）.
type AlertItem struct {
	AlertType string   `json:"alertType"`
	Regions   []string `json:"regions"`
}

// AlertsView R4 的回應；subscriptions 永遠是陣列.
type AlertsView struct {
	Subscriptions []AlertItem `json:"subscriptions"`
}

// CountyView R8 的一個縣市.
type CountyView struct {
	Name      string   `json:"name"`
	Townships []string `json:"townships"`
}

// RegionsView R8 的回應.
type RegionsView struct {
	Counties []CountyView `json:"counties"`
}

func validAlertType(t string) bool {
	return t == alertsub.TypeWeatherWarning || t == alertsub.TypeEarthquakeTsunami
}

// subError 把訂閱服務的錯誤對應成 HTTP 回應：已刪除 410、補刪失敗 500/5002、參數 400、其餘 500/5001.
func subError(c echo.Context, what string, err error) error {
	switch {
	case errors.Is(err, subscription.ErrErased):
		return erasedResponse(c)
	case errors.Is(err, subscription.ErrEraseIncomplete):
		return eraseIncompleteResponse(c)
	case errors.Is(err, subscription.ErrInvalidRegions), errors.Is(err, subscription.ErrInvalidType):
		return badRequest(c, "invalid regions or alert type")
	}
	return serverError(c, what, err)
}

// ListAlerts GET /me/alerts：列出自己開啟中的通知（F30-05）.
func (h *Handler) ListAlerts(c echo.Context) error {
	if h.subs == nil {
		return notConfigured(c)
	}
	items, err := h.subs.List(c.Request().Context(), userID(c))
	if err != nil {
		return serverError(c, "list alerts", err)
	}
	view := AlertsView{Subscriptions: []AlertItem{}}
	for _, it := range items {
		if it.Enabled {
			view.Subscriptions = append(view.Subscriptions, AlertItem{AlertType: it.AlertType, Regions: nonNil(it.Regions)})
		}
	}
	return httputil.OK(c, view)
}

// DeleteAlert DELETE /me/alerts/:type：關閉自己的一項通知；天氣特報整類（所有地區）一起關，不存在也回 204（F30-05）.
func (h *Handler) DeleteAlert(c echo.Context) error {
	if h.subs == nil {
		return notConfigured(c)
	}
	t := c.Param("type")
	if !validAlertType(t) {
		return badRequest(c, "invalid alert type")
	}
	if err := h.subs.Disable(c.Request().Context(), userID(c), t); err != nil {
		return subError(c, "disable alert", err)
	}
	return c.NoContent(http.StatusNoContent)
}

// alertBody R7 body：regions 先留原始 JSON，才分得出「缺」「null」「null 元素」「非字串元素」.
type alertBody struct {
	Regions json.RawMessage `json:"regions"`
}

// parseAlertBody [F30-06] 嚴格解析：有 body 時 Content-Type 必須是 application/json；DisallowUnknownFields；尾端不得有多餘內容.
// 回傳 hasRegions 與（存在時的）regions；任何不合回 ok=false.
func parseAlertBody(c echo.Context) (regions []string, hasRegions, ok bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxAlertBodyBytes+1))
	if err != nil || len(body) > maxAlertBodyBytes {
		return nil, false, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false, true
	}
	if mt, _, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType)); err != nil || mt != echo.MIMEApplicationJSON {
		return nil, false, false
	}
	if trimmed[0] != '{' { // null、陣列、字串等都不是物件
		return nil, false, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var in alertBody
	if err := dec.Decode(&in); err != nil {
		return nil, false, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // 尾端多餘內容
		return nil, false, false
	}
	if len(in.Regions) == 0 {
		return nil, false, true
	}
	var elems []any
	if bytes.Equal(bytes.TrimSpace(in.Regions), []byte("null")) || json.Unmarshal(in.Regions, &elems) != nil || elems == nil {
		return nil, true, false
	}
	regions = make([]string, 0, len(elems))
	for _, e := range elems {
		s, isString := e.(string)
		if !isString {
			return nil, true, false
		}
		regions = append(regions, s)
	}
	return regions, true, true
}

// PutAlert PUT /me/alerts/:type：開啟（或改地區）自己的一項通知，冪等、整份取代（F30-06）.
func (h *Handler) PutAlert(c echo.Context) error {
	// 收到請求的時間（毫秒）：標記不早於它 = 使用者在這次請求送出之後刪除了資料.
	since := h.now().Truncate(time.Millisecond)
	if h.subs == nil {
		return notConfigured(c)
	}
	t := c.Param("type")
	if !validAlertType(t) {
		return badRequest(c, "invalid alert type")
	}
	regions, hasRegions, ok := parseAlertBody(c)
	if !ok {
		return badRequest(c, "invalid body")
	}
	ctx := c.Request().Context()
	if t == alertsub.TypeEarthquakeTsunami {
		if hasRegions {
			return badRequest(c, "earthquake_tsunami takes no regions")
		}
		if err := h.subs.EnableQuakeTsunami(ctx, userID(c), since); err != nil {
			return subError(c, "enable earthquake tsunami", err)
		}
		return httputil.OK(c, AlertItem{AlertType: t, Regions: []string{}})
	}
	if !hasRegions {
		return badRequest(c, "regions required")
	}
	// 伺服器端白名單 + 正規化（與服務同一個函式）：回應就是實際存下的值.
	stored, err := subscription.NormalizeRegions(regions)
	if err != nil {
		return subError(c, "normalize regions", err)
	}
	if err := h.subs.SetWeatherWarningRegions(ctx, userID(c), stored, since); err != nil {
		return subError(c, "set weather warning regions", err)
	}
	return httputil.OK(c, AlertItem{AlertType: t, Regions: stored})
}

// Regions GET /me/regions：縣市 → 鄉鎮清單（靜態資料，與 R7 的驗證同一份 region 資料；F30-06）.
func (h *Handler) Regions(c echo.Context) error {
	view := RegionsView{Counties: make([]CountyView, 0, len(region.Counties))}
	for _, name := range region.Counties {
		view.Counties = append(view.Counties, CountyView{Name: name, Townships: nonNil(region.Townships(name))})
	}
	return httputil.OK(c, view)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
