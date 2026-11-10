package subscription

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
)

// F30-01 的測試：防寫回、錯誤分類、地區正規化、List.

type fakeStore struct {
	docs      map[string]alertsub.AlertSubscription
	upserts   int
	deletes   int
	upsertErr error
	deleteErr error
	// afterUpsert 在 Upsert 之後（模擬刪除夾在寫入之中）.
	afterUpsert func()
	// deleteCtxErr 記下 Delete 收到的 ctx 當下是否已取消.
	deleteCtxErr error
}

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string]alertsub.AlertSubscription{}} }

func (f *fakeStore) Upsert(_ context.Context, sub *alertsub.AlertSubscription) error {
	f.upserts++
	if f.upsertErr == nil {
		f.docs[sub.ID] = *sub
	}
	if f.afterUpsert != nil {
		f.afterUpsert()
	}
	return f.upsertErr
}

func (f *fakeStore) Delete(ctx context.Context, id string) error {
	f.deletes++
	f.deleteCtxErr = ctx.Err()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.docs, id)
	return nil
}

// listStore 多一個 FindByUserID，所以是 Lister.
type listStore struct{ *fakeStore }

func (l listStore) FindByUserID(_ context.Context, userID string) ([]alertsub.AlertSubscription, error) {
	var out []alertsub.AlertSubscription
	for _, d := range l.docs {
		if d.UserID == userID {
			out = append(out, d)
		}
	}
	return out, nil
}

type fakeErased struct {
	answers []bool // 依序回答；用完回最後一個
	calls   []time.Time
}

func (f *fakeErased) ErasedSince(_ context.Context, _ string, since time.Time) bool {
	f.calls = append(f.calls, since)
	i := min(len(f.calls)-1, len(f.answers)-1)
	return f.answers[i]
}

const uid = "U0123456789abcdef0123456789abcdef"

func TestWriteGuardsAgainstErasure(t *testing.T) {
	since := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	storeErr := errors.New("mongo down")
	deleteErr := errors.New("delete failed")
	enable := map[string]func(s *DefaultService) error{
		"quake": func(s *DefaultService) error { return s.EnableQuakeTsunami(t.Context(), uid, since) },
		"weather": func(s *DefaultService) error {
			return s.SetWeatherWarningRegions(t.Context(), uid, []string{"台北市"}, since)
		},
	}
	cases := []struct {
		name        string
		answers     []bool
		upsertErr   error
		deleteErr   error
		wantIs      error // 期望 errors.Is 為真
		wantNotIs   []error
		wantUpserts int
		wantDeletes int
		wantDoc     bool
	}{
		{name: "ok", answers: []bool{false}, wantUpserts: 1, wantDoc: true},
		{name: "blocked before write", answers: []bool{true}, wantIs: ErrErased, wantUpserts: 0},
		{name: "erased during write, undo ok", answers: []bool{false, true}, wantIs: ErrErased, wantUpserts: 1, wantDeletes: 1},
		{
			name: "erased during write, upsert failed", answers: []bool{false, true}, upsertErr: storeErr,
			wantIs: ErrErased, wantNotIs: []error{storeErr}, wantUpserts: 1, wantDeletes: 1,
		},
		{
			name: "erased during write, undo fails", answers: []bool{false, true}, deleteErr: deleteErr,
			wantIs: ErrEraseIncomplete, wantNotIs: []error{ErrErased}, wantUpserts: 1, wantDeletes: 1, wantDoc: true,
		},
		{
			name: "plain store error wrapped", answers: []bool{false}, upsertErr: storeErr,
			wantIs: storeErr, wantNotIs: []error{ErrErased, ErrEraseIncomplete}, wantUpserts: 1,
		},
	}
	for kind, run := range enable {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				st := newFakeStore()
				st.upsertErr, st.deleteErr = tc.upsertErr, tc.deleteErr
				er := &fakeErased{answers: tc.answers}
				err := run(New(st).WithErasureCheck(er))
				if tc.wantIs == nil && err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Fatalf("err = %v, want Is %v", err, tc.wantIs)
				}
				for _, n := range tc.wantNotIs {
					if errors.Is(err, n) {
						t.Fatalf("err = %v must not be %v", err, n)
					}
				}
				if err != nil && (strings.Contains(err.Error(), uid) || strings.Contains(err.Error(), "台北市")) {
					t.Fatalf("error text leaks userID or region: %q", err)
				}
				if st.upserts != tc.wantUpserts || st.deletes != tc.wantDeletes {
					t.Fatalf("upserts=%d deletes=%d, want %d/%d", st.upserts, st.deletes, tc.wantUpserts, tc.wantDeletes)
				}
				if _, ok := st.docs[uid+"_"+map[string]string{"quake": alertsub.TypeEarthquakeTsunami, "weather": alertsub.TypeWeatherWarning}[kind]]; ok != tc.wantDoc {
					t.Fatalf("doc present = %v, want %v", ok, tc.wantDoc)
				}
				for _, got := range er.calls {
					if !got.Equal(since) {
						t.Fatalf("since passed = %v, want %v", got, since)
					}
				}
			})
		}
	}
}

func TestUndoRunsDetachedFromCancelledRequest(t *testing.T) {
	st := newFakeStore()
	ctx, cancel := context.WithCancel(t.Context())
	er := &fakeErased{answers: []bool{false, true}}
	st.afterUpsert = cancel // 請求在寫入之後被取消
	err := New(st).WithErasureCheck(er).EnableQuakeTsunami(ctx, uid, time.Now())
	if !errors.Is(err, ErrErased) || st.deletes != 1 {
		t.Fatalf("err=%v deletes=%d", err, st.deletes)
	}
	if st.deleteCtxErr != nil {
		t.Fatalf("undo ctx was cancelled with the request: %v", st.deleteCtxErr)
	}
}

type undoCtxChecker struct {
	fakeErased
	used bool
}

func (u *undoCtxChecker) UndoContext(ctx context.Context) (context.Context, context.CancelFunc) {
	u.used = true
	return context.WithTimeout(context.WithoutCancel(ctx), time.Second)
}

func TestUndoUsesCheckerContextWhenProvided(t *testing.T) {
	st := newFakeStore()
	er := &undoCtxChecker{fakeErased: fakeErased{answers: []bool{false, true}}}
	if err := New(st).WithErasureCheck(er).EnableQuakeTsunami(t.Context(), uid, time.Now()); !errors.Is(err, ErrErased) {
		t.Fatalf("err = %v", err)
	}
	if !er.used {
		t.Fatal("UndoContext not used for the undo")
	}
}

func TestNilErasureCheckSkipsGuards(t *testing.T) {
	st := newFakeStore()
	if err := New(st).EnableQuakeTsunami(t.Context(), uid, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st.upserts != 1 || st.deletes != 0 {
		t.Fatalf("upserts=%d deletes=%d", st.upserts, st.deletes)
	}
}

func TestNormalizeRegions(t *testing.T) {
	dup401 := slices.Repeat([]string{"台北市"}, 401)
	dup400 := slices.Repeat([]string{"台北市"}, 400)
	cases := []struct {
		name string
		in   []string
		want []string
		err  error
	}{
		{
			"reversed order, 臺, dedupe, whole county eats towns",
			[]string{"新北市板橋區", "臺北市", "台北市大安區", "台北市", "新北市板橋區"},
			[]string{"台北市", "新北市板橋區"},
			nil,
		},
		{"order follows Counties then Townships", []string{"新北市板橋區", "台北市"}, []string{"台北市", "新北市板橋區"}, nil},
		{"townships follow region.Townships order", []string{"台北市中正區", "台北市大安區"}, []string{"台北市大安區", "台北市中正區"}, nil},
		{"400 raw duplicates ok", dup400, []string{"台北市"}, nil},
		{"401 raw duplicates rejected", dup401, nil, ErrInvalidRegions},
		{"empty", []string{}, nil, ErrInvalidRegions},
		{"nil", nil, nil, ErrInvalidRegions},
		{"unknown county", []string{"火星市"}, nil, ErrInvalidRegions},
		{"unknown township", []string{"台北市火星區"}, nil, ErrInvalidRegions},
		{"blank string", []string{""}, nil, ErrInvalidRegions},
		{"one bad among good", []string{"台北市", "bogus"}, nil, ErrInvalidRegions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRegions(tc.in)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMaxLegalRegionsBelowRawCap 實測 H2：每個縣市的 region.Areas 去重後加總必須小於原始上限 400.
func TestMaxLegalRegionsBelowRawCap(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range region.Counties {
		for _, a := range region.Areas(c) {
			seen[a] = true
		}
	}
	t.Logf("distinct legal regions = %d", len(seen))
	if len(seen) >= MaxRawRegions {
		t.Fatalf("legal max %d is not below raw cap %d", len(seen), MaxRawRegions)
	}
	all := make([]string, 0, len(seen))
	for a := range seen {
		all = append(all, a)
	}
	if _, err := NormalizeRegions(all); err != nil {
		t.Fatalf("all legal regions rejected: %v", err)
	}
}

func TestSetWeatherWarningStoresNormalizedOrder(t *testing.T) {
	st := newFakeStore()
	s := New(st)
	if err := s.SetWeatherWarningRegions(t.Context(), uid, []string{"新北市板橋區", "台北市"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := st.docs[uid+"_weather_warning"].Regions; !slices.Equal(got, []string{"台北市", "新北市板橋區"}) {
		t.Fatalf("stored %v", got)
	}
}

func TestInvalidInputsNeverWrite(t *testing.T) {
	st := newFakeStore()
	s := New(st)
	if err := s.SetWeatherWarningRegions(t.Context(), uid, nil, time.Now()); !errors.Is(err, ErrInvalidRegions) {
		t.Fatalf("empty regions err = %v", err)
	}
	if err := s.Disable(t.Context(), uid, "bogus"); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("bogus type err = %v", err)
	}
	if st.upserts != 0 || st.deletes != 0 {
		t.Fatalf("wrote on invalid input: %d/%d", st.upserts, st.deletes)
	}
}

func TestDisable(t *testing.T) {
	st := newFakeStore()
	s := New(st)
	for _, typ := range []string{alertsub.TypeWeatherWarning, alertsub.TypeEarthquakeTsunami} {
		st.docs[uid+"_"+typ] = alertsub.AlertSubscription{ID: uid + "_" + typ}
	}
	st.docs["U2_weather_warning"] = alertsub.AlertSubscription{ID: "U2_weather_warning"}
	if err := s.Disable(t.Context(), uid, alertsub.TypeWeatherWarning); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(t.Context(), uid, alertsub.TypeWeatherWarning); err != nil {
		t.Fatalf("second disable must be idempotent: %v", err)
	}
	if _, ok := st.docs[uid+"_weather_warning"]; ok {
		t.Fatal("weather doc still there")
	}
	if _, ok := st.docs[uid+"_earthquake_tsunami"]; !ok {
		t.Fatal("quake doc removed")
	}
	if _, ok := st.docs["U2_weather_warning"]; !ok {
		t.Fatal("other user's doc removed")
	}
	st.deleteErr = errors.New("boom")
	if err := s.Disable(t.Context(), uid, alertsub.TypeEarthquakeTsunami); !errors.Is(err, st.deleteErr) {
		t.Fatalf("store error not wrapped: %v", err)
	}
}

func TestList(t *testing.T) {
	st := newFakeStore()
	st.docs["a"] = alertsub.AlertSubscription{UserID: uid, AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true}
	st.docs["b"] = alertsub.AlertSubscription{UserID: uid, AlertType: alertsub.TypeWeatherWarning, Enabled: false, Regions: []string{"台北市"}}
	st.docs["c"] = alertsub.AlertSubscription{UserID: uid, AlertType: "unknown_type", Enabled: true}
	st.docs["d"] = alertsub.AlertSubscription{UserID: "U2", AlertType: alertsub.TypeWeatherWarning, Enabled: true, Regions: []string{"新北市"}}
	got, err := New(listStore{st}).List(t.Context(), uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].AlertType != alertsub.TypeWeatherWarning || got[1].AlertType != alertsub.TypeEarthquakeTsunami {
		t.Fatalf("order/filter wrong: %+v", got)
	}
	if got[0].Enabled || !got[1].Enabled {
		t.Fatalf("enabled flags wrong: %+v", got)
	}
	if got[1].Regions == nil || len(got[1].Regions) != 0 {
		t.Fatalf("quake regions must be empty non-nil slice: %#v", got[1].Regions)
	}
	empty, err := New(listStore{newFakeStore()}).List(t.Context(), uid)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no subscriptions must be empty non-nil: %#v %v", empty, err)
	}
}

func TestListUnsupportedStoreIsExplicitError(t *testing.T) {
	got, err := New(newFakeStore()).List(t.Context(), uid)
	if !errors.Is(err, ErrListUnsupported) || got != nil {
		t.Fatalf("got %v, %v; want ErrListUnsupported and no items", got, err)
	}
}
