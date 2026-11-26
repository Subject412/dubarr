package arrrescan_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/Subject412/dubarr/backend/internal/arr"
	"github.com/Subject412/dubarr/backend/internal/arr/arrmock"
	"github.com/Subject412/dubarr/backend/internal/arrrescan"
	"github.com/Subject412/dubarr/backend/internal/secrets"
	"github.com/Subject412/dubarr/backend/internal/store"
)

// testHTTPTimeout bounds one request to the mock arr. It is a hang guard, not a
// performance assertion: a loaded runner slowing the mock down is not a defect.
const testHTTPTimeout = 30 * time.Second

func testCipher(t *testing.T) *secrets.Cipher {
	t.Helper()
	key := make([]byte, secrets.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random key: %v", err)
	}
	c, err := secrets.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// seedInstance inserts one arr_instances row the way the library sync will,
// encrypting the key with the same AES-GCM store used in production.
func seedInstance(t *testing.T, st *store.Store, c *secrets.Cipher, kind, baseURL, apiKey string, enabled bool) int64 {
	t.Helper()
	enc, err := c.EncryptString(apiKey)
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	last4 := apiKey
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	res, err := st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		kind, kind, baseURL, enc, last4, enabled)
	if err != nil {
		t.Fatalf("insert arr instance: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func newRescanner(t *testing.T, c *secrets.Cipher) (*arrrescan.Rescanner, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r, err := arrrescan.New(arrrescan.Deps{DB: st.DB, Secrets: c, Timeout: testHTTPTimeout})
	if err != nil {
		t.Fatalf("arrrescan.New: %v", err)
	}
	return r, st
}

func TestMovieRowRescansRadarr(t *testing.T) {
	ctx := context.Background()
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()
	id := seedInstance(t, st, c, "radarr", srv.URL, srv.APIKey(), true)

	if err := r.RescanAfterMux(ctx, arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "movie",
		ExternalItemID: 7,
	}); err != nil {
		t.Fatalf("RescanAfterMux: %v", err)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v, want one", cmds)
	}
	if want := (arrmock.CommandRequest{Name: "RescanMovie", MovieID: 7}); cmds[0] != want {
		t.Errorf("command = %+v, want %+v", cmds[0], want)
	}
	reqs := srv.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != "POST" || last.Path != "/api/v3/command" {
		t.Errorf("last request = %s %s", last.Method, last.Path)
	}
	if last.APIKey != srv.APIKey() {
		t.Errorf("X-Api-Key = %q, want the decrypted stored key", last.APIKey)
	}
}

func TestEpisodeRowRescansSonarrSeries(t *testing.T) {
	ctx := context.Background()
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
	defer srv.Close()
	id := seedInstance(t, st, c, "sonarr", srv.URL, srv.APIKey(), true)

	if err := r.RescanAfterMux(ctx, arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "episode",
		ExternalItemID: 44, // Sonarr episodeId, deliberately not the series id
		SeriesID:       3,
	}); err != nil {
		t.Fatalf("RescanAfterMux: %v", err)
	}

	cmds := srv.Commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %+v, want one", cmds)
	}
	if want := (arrmock.CommandRequest{Name: "RescanSeries", SeriesID: 3}); cmds[0] != want {
		t.Errorf("command = %+v, want %+v", cmds[0], want)
	}
}

func TestNoInstanceIsSkipped(t *testing.T) {
	c := testCipher(t)
	r, _ := newRescanner(t, c)

	err := r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{
		ArrInstanceID:  999,
		MediaType:      "movie",
		ExternalItemID: 7,
	})
	if !errors.Is(err, arrrescan.ErrNoInstance) {
		t.Fatalf("error = %v, want ErrNoInstance", err)
	}
}

func TestDisabledInstanceIsSkipped(t *testing.T) {
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()
	id := seedInstance(t, st, c, "radarr", srv.URL, srv.APIKey(), false)

	err := r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "movie",
		ExternalItemID: 7,
	})
	if !errors.Is(err, arrrescan.ErrNoInstance) {
		t.Fatalf("error = %v, want ErrNoInstance", err)
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("disabled instance saw %d requests, want 0", got)
	}
}

func TestInstanceKindMismatchIsAnError(t *testing.T) {
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Sonarr, "", arrmock.Fixtures{})
	defer srv.Close()
	id := seedInstance(t, st, c, "sonarr", srv.URL, srv.APIKey(), true)

	err := r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "movie",
		ExternalItemID: 7,
	})
	if !errors.Is(err, arrrescan.ErrInstanceKind) {
		t.Fatalf("error = %v, want ErrInstanceKind", err)
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("mismatched instance saw %d requests, want 0", got)
	}
}

func TestInvalidLibraryRowIsRejected(t *testing.T) {
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()
	id := seedInstance(t, st, c, "radarr", srv.URL, srv.APIKey(), true)

	tests := []arrrescan.LibraryCacheRow{
		{ArrInstanceID: 0, MediaType: "movie", ExternalItemID: 7},
		{ArrInstanceID: id, MediaType: "movie", ExternalItemID: 0},
		{ArrInstanceID: id, MediaType: "episode", ExternalItemID: 44, SeriesID: 0},
		{ArrInstanceID: id, MediaType: "series", ExternalItemID: 7},
	}
	for _, row := range tests {
		if err := r.RescanAfterMux(context.Background(), row); !errors.Is(err, arrrescan.ErrInvalidLibraryRow) {
			t.Errorf("row %+v error = %v, want ErrInvalidLibraryRow", row, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("invalid rows produced %d requests, want 0", got)
	}
}

func TestArrFailureIsReturned(t *testing.T) {
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()
	srv.FailAll(500)
	id := seedInstance(t, st, c, "radarr", srv.URL, srv.APIKey(), true)

	err := r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "movie",
		ExternalItemID: 7,
	})
	if err == nil {
		t.Fatal("RescanAfterMux returned nil on a failing instance")
	}
	if errors.Is(err, arrrescan.ErrNoInstance) {
		t.Fatalf("error = %v, must not be ErrNoInstance", err)
	}
	if !errors.Is(err, arr.ErrUnexpectedStatus) {
		t.Fatalf("error = %v, want arr.ErrUnexpectedStatus", err)
	}
}

func TestNewRequiresDatabaseAndSecrets(t *testing.T) {
	if _, err := arrrescan.New(arrrescan.Deps{}); err == nil {
		t.Error("New accepted empty deps")
	}
	if _, err := arrrescan.New(arrrescan.Deps{DB: nil, Secrets: testCipher(t)}); err == nil {
		t.Error("New accepted nil DB")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if _, err := arrrescan.New(arrrescan.Deps{DB: st.DB}); err == nil {
		t.Error("New accepted nil secrets")
	}
}

// TestUnreadableKeyReturnsError proves a corrupt ciphertext fails loudly and is
// never sent to the instance as an empty key.
func TestUnreadableKeyReturnsError(t *testing.T) {
	c := testCipher(t)
	r, st := newRescanner(t, c)

	srv := arrmock.New(arrmock.Radarr, "", arrmock.Fixtures{})
	defer srv.Close()
	res, err := st.DB.Exec(
		`INSERT INTO arr_instances (name, type, base_url, api_key_enc, api_key_last4, enabled)
		 VALUES ('radarr', 'radarr', ?, ?, 'last', 1)`, srv.URL, []byte("not-a-valid-blob"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()

	err = r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{
		ArrInstanceID:  id,
		MediaType:      "movie",
		ExternalItemID: 7,
	})
	if err == nil {
		t.Fatal("RescanAfterMux accepted a corrupt API key blob")
	}
	if got := len(srv.Requests()); got != 0 {
		t.Errorf("corrupt key produced %d requests, want 0", got)
	}
}

func TestRescannerIsNilSafe(t *testing.T) {
	var r *arrrescan.Rescanner
	if err := r.RescanAfterMux(context.Background(), arrrescan.LibraryCacheRow{}); err == nil {
		t.Error("nil Rescanner returned nil")
	}
}
