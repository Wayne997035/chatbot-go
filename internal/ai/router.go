package ai

import (
	"context"
	"errors"
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
	down  map[providerKey]time.Time

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

type listKey struct {
	provider   string
	capability Capability
}

// providerKey 供應商層冷卻的鍵：文字與看圖分開，一種能力故障不牽連另一種.
type providerKey struct {
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
		down:      make(map[providerKey]time.Time),
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
	for _, name := range r.coolestLast(order, req.Capability()) {
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

// coolestLast 把冷卻中的供應商排到最後，兩組各自保持原順序（全部冷卻時仍會依序嘗試）.
//
// [F33-04] 只是「不先試」而不是完全跳過：三家都冷卻或其他家都失敗時，請求仍有機會成功.
func (r *Router) coolestLast(order []string, c Capability) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	ready := make([]string, 0, len(order))
	var cooled []string
	for _, name := range order {
		if until, ok := r.down[providerKey{name, c}]; ok && now.Before(until) {
			cooled = append(cooled, name)
			continue
		}
		ready = append(ready, name)
	}
	return append(ready, cooled...)
}

// walk 嘗試一家供應商的候選模型；回傳空字串表示這家沒有可用回答.
func (r *Router) walk(ctx context.Context, p Provider, req *Request, res *Result) (text, model string) {
	capability := req.Capability()
	// 所有時間比較都用 r.now 這個時鐘；呼叫端 ctx 的期限換算成「還剩多久」再放進來.
	attemptTimeout, budget := r.settings.AttemptTimeout, r.settings.ProviderBudget
	if lp, ok := p.(limitsProvider); ok {
		// [F33-07] Claude 自帶 25s/25s：付費問答模型回答較慢，不套 8s/10s；呼叫端期限仍在下面優先.
		attemptTimeout, budget = lp.Limits()
	}
	deadline := r.now().Add(budget)
	if d, ok := ctx.Deadline(); ok {
		if callerDeadline := r.now().Add(time.Until(d)); callerDeadline.Before(deadline) {
			deadline = callerDeadline
		}
	}
	tried := make(map[string]bool)
	waited := make(map[string]bool)
	for len(tried) < r.settings.MaxAttempts {
		model = r.nextModel(ctx, p, capability, tried, res, attemptTimeout, deadline)
		if model == "" {
			return "", ""
		}
		if !r.now().Before(deadline) {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: 超過 %s 時間上限，換下一家", p.Name(), budget))
			return "", ""
		}
		tried[model] = true
		text, err := r.attempt(ctx, p, model, req, attemptTimeout, deadline)
		if err == nil {
			r.promote(p.Name(), capability, model)
			return text, model
		}
		slog.Warn("AI attempt failed", "detail", shortError(p.Name(), model, err))
		res.Errors = append(res.Errors, shortError(p.Name(), model, err))
		r.noteProviderDown(ctx, p.Name(), capability, err)
		if ctx.Err() != nil {
			return "", ""
		}
		if r.handleFailure(ctx, p.Name(), capability, model, err, deadline, waited) {
			delete(tried, model) // 已等過限流，再試同一個模型.
		}
	}
	return "", ""
}

// callTimeout 單次呼叫的逾時：不超過 AttemptTimeout，也不超過這家剩下的預算（含呼叫端期限）.
//
// [F33-03] 以前單次 = 整家剩餘預算，一個卡住的模型就吃光整家時間；用剩餘時間（而不是絕對時刻）
// 設定逾時，因為 deadline 是以 r.now 計算的，不一定等於系統時鐘.
func (r *Router) callTimeout(attemptTimeout time.Duration, deadline time.Time) time.Duration {
	return min(attemptTimeout, deadline.Sub(r.now()))
}

// attempt 呼叫一次模型並檢查回答.
func (r *Router) attempt(
	ctx context.Context, p Provider, model string, req *Request, attemptTimeout time.Duration, deadline time.Time,
) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.callTimeout(attemptTimeout, deadline))
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
func (r *Router) nextModel(
	ctx context.Context, p Provider, c Capability, tried map[string]bool, res *Result, attemptTimeout time.Duration, deadline time.Time,
) string {
	for _, m := range r.candidates(ctx, p, c, res, attemptTimeout, deadline) {
		if !tried[m] {
			return m
		}
	}
	return ""
}

func (r *Router) candidates(
	ctx context.Context, p Provider, c Capability, res *Result, attemptTimeout time.Duration, deadline time.Time,
) []string {
	// [F33-01] 營運者指定了清單：照清單順序，不列模型、不快取、不 promote.
	if list := r.settings.Models[p.Name()]; len(list) > 0 {
		return r.configuredCandidates(p, c, list)
	}
	key := listKey{p.Name(), c}
	r.mu.Lock()
	cached := r.lists[key]
	r.mu.Unlock()

	if cached == nil || r.now().After(cached.expires) {
		// [F33-03] 列模型也會卡住，同樣受單次上限約束；逾時視為供應商故障.
		listCtx, cancel := context.WithTimeout(ctx, r.callTimeout(attemptTimeout, deadline))
		models, err := p.Candidates(listCtx, c)
		cancel()
		if err != nil {
			res.Errors = append(res.Errors, shortError(p.Name(), "list", err))
			r.noteProviderDown(ctx, p.Name(), c, err)
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

// configuredCandidates 清單模式的候選：原順序扣掉冷卻中的模型；看圖請求再濾掉不能看圖的.
// 過濾後為空就是這家沒有候選，NEVER 退回自動排名.
func (r *Router) configuredCandidates(p Provider, c Capability, list []string) []string {
	checker, _ := p.(visionChecker)
	r.mu.Lock()
	defer r.mu.Unlock()
	usable := make([]string, 0, len(list))
	for _, m := range list {
		if c == Vision && checker != nil && !checker.SupportsVision(m) {
			continue
		}
		if until, dead := r.dead[modelKey{p.Name(), c, m}]; dead && r.now().Before(until) {
			continue
		}
		usable = append(usable, m)
	}
	return usable
}

// callerDeadlineSlack [F33-35] 單次期限由呼叫端期限換算而來（router.go walk 兩次取時間），
// 子 ctx 可能比呼叫端 ctx 早幾 ns~µs 到期，這時呼叫端 ctx 的計時器還沒跑、ctx.Err() 仍是 nil.
// 實測（真時鐘、呼叫端期限 20ms）會誤記冷卻，所以「期限已過」要容許這點換算誤差.
const callerDeadlineSlack = 5 * time.Millisecond

// callerDeadlineReached [F33-35] 失敗是不是呼叫端造成的：ctx 已結束，或這次失敗是逾時且呼叫端期限已到（含換算誤差）.
// 只放寬逾時類錯誤；期限附近回的 5xx 等仍算供應商的錯.
func callerDeadlineReached(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && errors.Is(err, context.DeadlineExceeded) && time.Until(d) <= callerDeadlineSlack
}

// noteProviderDown 供應商端故障才冷卻整家（鍵含能力）；呼叫端取消或期限到不是供應商的錯，不記.
func (r *Router) noteProviderDown(ctx context.Context, provider string, c Capability, err error) {
	if callerDeadlineReached(ctx, err) || !providerDown(err) {
		return
	}
	r.markProviderDown(provider, c, r.settings.ProviderCooldown)
}

// markProviderDown 自己取鎖：多個請求同時觸發時以 r.mu 序列化.
func (r *Router) markProviderDown(provider string, c Capability, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down[providerKey{provider, c}] = r.now().Add(cooldown)
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
