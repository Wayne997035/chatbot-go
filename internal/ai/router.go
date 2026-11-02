package ai

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Router 依序嘗試各供應商與其候選模型，直到取得可用回答.
type Router struct {
	providers map[string]Provider
	settings  Settings

	mu    sync.Mutex
	lists map[listKey]*candidateList
	dead  map[modelKey]time.Time

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

type listKey struct {
	provider   string
	capability Capability
}

type modelKey struct {
	provider   string
	capability Capability
	model      string
}

type candidateList struct {
	models  []string
	expires time.Time
}

// NewRouter 建立路由；providers 中為 nil 的會被忽略（未設定金鑰）.
func NewRouter(settings *Settings, providers ...Provider) *Router {
	r := &Router{
		providers: make(map[string]Provider),
		settings:  settings.withDefaults(),
		lists:     make(map[listKey]*candidateList),
		dead:      make(map[modelKey]time.Time),
		now:       time.Now,
		sleep:     sleepContext,
	}
	for _, p := range providers {
		if p != nil {
			r.providers[p.Name()] = p
		}
	}
	return r
}

// Configured 回傳已設定金鑰的供應商（依名稱排序）.
func (r *Router) Configured() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Complete 依序嘗試各供應商，回傳第一個可用回答；全部失敗時 Result.OK() 為 false. 不會 panic 也不回傳 error.
func (r *Router) Complete(ctx context.Context, req *Request) Result {
	var res Result
	order := r.settings.TextOrder
	if req.Capability() == Vision {
		order = r.settings.VisionOrder
	}
	if len(req.Order) > 0 {
		order = req.Order
	}
	for _, name := range order {
		p, ok := r.providers[name]
		if !ok {
			continue
		}
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "請求已逾時或取消")
			break
		}
		if text, model := r.walk(ctx, p, req, &res); text != "" {
			res.Text, res.Provider, res.Model = text, name, model
			return res
		}
	}
	if len(r.providers) == 0 {
		res.Errors = append(res.Errors, "沒有可用的 AI 供應商（未設定 API key）")
	}
	return res
}

// walk 嘗試一家供應商的候選模型；回傳空字串表示這家沒有可用回答.
func (r *Router) walk(ctx context.Context, p Provider, req *Request, res *Result) (text, model string) {
	capability := req.Capability()
	deadline := r.now().Add(r.settings.ProviderBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	tried := make(map[string]bool)
	waited := make(map[string]bool)
	for len(tried) < r.settings.MaxAttempts {
		model = r.nextModel(ctx, p, capability, tried, res)
		if model == "" {
			return "", ""
		}
		if !r.now().Before(deadline) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: 超過 %s 時間上限，換下一家", p.Name(), r.settings.ProviderBudget))
			return "", ""
		}
		tried[model] = true
		text, err := r.attempt(ctx, p, model, req, deadline)
		if err == nil {
			r.promote(p.Name(), capability, model)
			return text, model
		}
		slog.Warn("AI attempt failed", "detail", shortError(p.Name(), model, err))
		res.Errors = append(res.Errors, shortError(p.Name(), model, err))
		if ctx.Err() != nil {
			return "", ""
		}
		if r.handleFailure(ctx, p.Name(), capability, model, err, deadline, waited) {
			delete(tried, model) // 已等過限流，再試同一個模型.
		}
	}
	return "", ""
}

// attempt 呼叫一次模型並檢查回答；單次逾時不超過這家剩下的預算.
func (r *Router) attempt(ctx context.Context, p Provider, model string, req *Request, deadline time.Time) (string, error) {
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	text, err := p.Call(callCtx, model, req)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(stripReasoning(text))
	if text == "" {
		return "", ErrEmpty
	}
	if req.Accept != nil && !req.Accept(text) {
		return "", ErrRejected
	}
	return text, nil
}

// handleFailure 處理失敗：不可用的模型進入冷卻；短暫限流則原地等待並回傳 true（重試同模型）.
func (r *Router) handleFailure(
	ctx context.Context, provider string, c Capability, model string, err error, deadline time.Time, waited map[string]bool,
) bool {
	if cooldown := unusableFor(err); cooldown > 0 {
		r.markDead(provider, c, model, cooldown)
		return false
	}
	wait, ok := retryWait(err)
	if !ok || waited[model] || wait > r.settings.RateLimitWait || !r.now().Add(wait).Before(deadline) {
		return false
	}
	waited[model] = true
	slog.Info("AI rate limited, waiting before retrying the same model", "provider", provider, "model", model, "wait", wait)
	return r.sleep(ctx, wait) == nil
}

// nextModel 回傳下一個尚未嘗試且未冷卻的候選模型.
func (r *Router) nextModel(ctx context.Context, p Provider, c Capability, tried map[string]bool, res *Result) string {
	for _, m := range r.candidates(ctx, p, c, res) {
		if !tried[m] {
			return m
		}
	}
	return ""
}

func (r *Router) candidates(ctx context.Context, p Provider, c Capability, res *Result) []string {
	key := listKey{p.Name(), c}
	r.mu.Lock()
	cached := r.lists[key]
	r.mu.Unlock()

	if cached == nil || r.now().After(cached.expires) {
		models, err := p.Candidates(ctx, c)
		if err != nil {
			res.Errors = append(res.Errors, shortError(p.Name(), "list", err))
			return nil
		}
		slog.Info("AI candidates", "provider", p.Name(), "capability", c.String(), "models", strings.Join(first(models, 8), ", "))
		cached = &candidateList{models: models, expires: r.now().Add(r.settings.CandidateTTL)}
		r.mu.Lock()
		r.lists[key] = cached
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	usable := make([]string, 0, len(cached.models))
	for _, m := range cached.models {
		if until, dead := r.dead[modelKey{p.Name(), c, m}]; dead && r.now().Before(until) {
			continue
		}
		usable = append(usable, m)
	}
	return usable
}

func (r *Router) markDead(provider string, c Capability, model string, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dead[modelKey{provider, c, model}] = r.now().Add(cooldown)
}

// promote 把成功的模型移到最前面，其他候選保留當備援.
func (r *Router) promote(provider string, c Capability, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.lists[listKey{provider, c}]
	if list == nil || len(list.models) == 0 || list.models[0] == model {
		return
	}
	reordered := []string{model}
	for _, m := range list.models {
		if m != model {
			reordered = append(reordered, m)
		}
	}
	list.models = reordered
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func stripReasoning(s string) string {
	return thinkBlock.ReplaceAllString(s, "")
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
