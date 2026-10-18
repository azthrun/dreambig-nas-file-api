package server_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/azthrun/dreambig-nas-file-api/internal/upload"
)

func TestFifthConcurrentUploadIsRefusedAndAcceptedAgainOnceOneCompletes(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{MaxActivePerCaller: 4})
	var targets []string
	for _, p := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		targets = append(targets, e.mustCreate(adminToken, 1, hello(p)))
	}

	rec := e.create(adminToken, 1, hello("e.txt"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fifth POST = %d, want 429; body %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "4 active Uploads") {
		t.Errorf("body %q does not explain the limit", rec.Body)
	}
	lines := auditLines(t, e)
	refused := lines[len(lines)-1]
	if refused["caller"] != "admin" || refused["operation"] != "upload_create" || refused["path"] != "/e.txt" ||
		refused["outcome"] != "too_many_uploads" || refused["status"] != float64(429) {
		t.Errorf("refused create line = %v", refused)
	}

	if rec := e.patch(adminToken, targets[0], 0, []byte("x")); rec.Code != http.StatusNoContent {
		t.Fatalf("completing PATCH = %d, want 204", rec.Code)
	}
	if rec := e.create(adminToken, 1, hello("e.txt")); rec.Code != http.StatusCreated {
		t.Fatalf("POST after one completed = %d, want 201; body %s", rec.Code, rec.Body)
	}
}

const gb = 1_000_000_000

// freeSpace fakes a volume with n bytes available.
func freeSpace(n int64) func() (int64, error) {
	return func() (int64, error) { return n, nil }
}

func TestUploadThatWouldEatIntoTheFreeSpaceReserveIsRefused(t *testing.T) {
	limits := upload.Limits{FreeSpaceReserve: 20 * gb, FreeSpace: freeSpace(100 * gb)}

	e := newEnvWith(t, t.TempDir(), limits)
	rec := e.create(adminToken, 80*gb+1, hello("too-big.bin"))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST = %d, want 507; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "free-space reserve") {
		t.Errorf("body %q does not explain the refusal", rec.Body)
	}
	if strings.Contains(rec.Body.String(), e.root) {
		t.Errorf("error echoes the NAS path: %s", rec.Body)
	}
	lines := auditLines(t, e)
	refused := lines[len(lines)-1]
	if refused["caller"] != "admin" || refused["operation"] != "upload_create" || refused["path"] != "/too-big.bin" ||
		refused["outcome"] != "insufficient_space" || refused["status"] != float64(507) {
		t.Errorf("refused create line = %v", refused)
	}

	if rec := e.create(adminToken, 80*gb, hello("fits.bin")); rec.Code != http.StatusCreated {
		t.Fatalf("POST leaving exactly the reserve = %d, want 201; body %s", rec.Code, rec.Body)
	}
}

// Free space is what is left once every active Upload has all its bytes, so two
// Uploads that each fit cannot together eat into the reserve.
func TestActiveUploadsBytesStillToComeCountAgainstFreeSpace(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{FreeSpaceReserve: 20 * gb, FreeSpace: freeSpace(100 * gb)})
	target := e.mustCreate(adminToken, 50*gb, hello("a.bin"))
	e.patch(adminToken, target, 0, []byte("some bytes"))
	if rec := e.create(backupToken, 31*gb, hello("backups/b.bin")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("second POST = %d, want 507", rec.Code)
	}
	if rec := e.create(backupToken, 30*gb, hello("backups/b.bin")); rec.Code != http.StatusCreated {
		t.Fatalf("second POST that fits = %d, want 201; body %s", rec.Code, rec.Body)
	}
}

func TestFailingToReadFreeSpaceRefusesTheUpload(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{FreeSpaceReserve: 20 * gb, FreeSpace: func() (int64, error) {
		return 0, errors.New("statfs /volume1/uploads: input/output error")
	}})
	rec := e.create(adminToken, 1, hello("a.txt"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "volume1") {
		t.Errorf("error echoes the NAS path: %s", rec.Body)
	}
}

// The real volume's free space is read when no fake is given.
func TestUploadRootVolumeFreeSpaceIsChecked(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{FreeSpaceReserve: 1 << 62})
	if rec := e.create(adminToken, 1, hello("a.txt")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST with an impossible reserve = %d, want 507", rec.Code)
	}
}

func TestCancellingAnUploadFreesItsSlot(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{MaxActivePerCaller: 1})
	target := e.mustCreate(adminToken, 2, hello("a.txt"))
	if rec := e.create(adminToken, 1, hello("b.txt")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second POST = %d, want 429", rec.Code)
	}
	if rec := e.tus(adminToken, http.MethodDelete, target, nil, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", rec.Code)
	}
	if rec := e.create(adminToken, 1, hello("b.txt")); rec.Code != http.StatusCreated {
		t.Fatalf("POST after cancelling = %d, want 201", rec.Code)
	}
}

func TestTheUploadLimitIsPerCaller(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{MaxActivePerCaller: 1})
	e.mustCreate(adminToken, 2, hello("backups/a.txt"))
	if rec := e.create(backupToken, 1, hello("backups/b.txt")); rec.Code != http.StatusCreated {
		t.Fatalf("other Caller's POST = %d, want 201; body %s", rec.Code, rec.Body)
	}
}

// Active Uploads are read from the Staging Area, so a restart does not reset the count.
func TestActiveUploadsStillCountAfterARestart(t *testing.T) {
	dir := t.TempDir()
	limits := upload.Limits{MaxActivePerCaller: 1}
	newEnvWith(t, dir, limits).mustCreate(adminToken, 2, hello("a.txt"))
	if rec := newEnvWith(t, dir, limits).create(adminToken, 1, hello("b.txt")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("POST after restart = %d, want 429", rec.Code)
	}
}

func TestConcurrentCreatesNeverExceedTheLimit(t *testing.T) {
	e := newEnvWith(t, t.TempDir(), upload.Limits{MaxActivePerCaller: 4})
	const n = 10
	codes := make(chan int, n)
	for i := range n {
		go func() { codes <- e.create(adminToken, 1, hello(fmt.Sprintf("f%d.txt", i))).Code }()
	}
	created := 0
	for range n {
		if <-codes == http.StatusCreated {
			created++
		}
	}
	if created != 4 {
		t.Fatalf("%d of %d concurrent POSTs were accepted, want 4", created, n)
	}
}
