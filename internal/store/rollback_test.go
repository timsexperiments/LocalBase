package store

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const olderVersionEnv = "LOCALBASE_TEST_OLDER_VERSION"

// An older version of LocalBase opens the database as SQLite does, with no record of whose -wal is
// beside it. If it's killed with commits in its -wal, this version refuses that -wal until the record
// goes, which is what a rollback does. Then it opens, with every write, including the older one's.
func TestAnOlderVersionsCrashOpensOnceTheRecordIsGone(t *testing.T) {
	if path := os.Getenv(olderVersionEnv); path != "" {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			fmt.Println("FAILED", err)
			return
		}
		_, err = db.Exec(`INSERT INTO go_settings (key, value, updated_at) VALUES ('older', 'kept', '2026-11-05T00:00:00.000Z')`)
		if err != nil {
			fmt.Println("FAILED", err)
			return
		}
		fmt.Println("WROTE")
		_, _ = io.ReadAll(os.Stdin) // killed while it waits
		return
	}
	path := filepath.Join(t.TempDir(), "local-base.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetSetting(ctx, "newer", "kept"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAnOlderVersionsCrashOpensOnceTheRecordIsGone$")
	cmd.Env = append(os.Environ(), olderVersionEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "WROTE" {
		t.Fatalf("the older version: %q %v", line, err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the crash should leave commits in a -wal: %v", err)
	}

	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("it opened with the record still there; this test no longer shows what it's for")
	}
	if err := ForgetWALOwner(path); err != nil {
		t.Fatal(err)
	}
	if err := ForgetWALOwner(path); err != nil {
		t.Errorf("a second time, with nothing to remove: %v", err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatalf("after the record went: %v", err)
	}
	defer st.Close()
	for _, key := range []string{"newer", "older"} {
		if value, ok, err := st.Setting(ctx, key); err != nil || !ok || value != "kept" {
			t.Errorf("%s: %q %v %v", key, value, ok, err)
		}
	}
}
