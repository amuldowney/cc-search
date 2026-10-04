//go:build unix

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type launchReceipt struct {
	Args                  []string
	Stdin, Stdout, Stderr string
}

func TestRefreshLauncherChild(t *testing.T) {
	socket := os.Getenv("CC_SEARCH_REFRESH_TEST_SOCKET")
	if socket == "" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	in, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Readlink("/proc/self/fd/1")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Readlink("/proc/self/fd/2")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(launchReceipt{args, in, out, stderr}); err != nil {
		t.Fatal(err)
	}
}

// A shell trampoline runs only the child test above; production reexecutes the
// cc-search binary. The socket handshake needs neither polling nor sleeps.
func TestRefreshLauncherPreservesOverridesAndDoesNotInheritStdio(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("stdio test uses Linux procfs")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "ready.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("CC_SEARCH_REFRESH_TEST_SOCKET", socket)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "refresh-helper")
	script := "#!/bin/sh\nexec " + strconv.Quote(testBinary) + " -test.run=^TestRefreshLauncherChild$ -- \"$@\"\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "index ?#+.db")
	roots := []string{filepath.Join(root, "root one"), filepath.Join(root, "root-two")}
	if err := startRefreshProcess(executable, path, roots); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { <-ctx.Done(); listener.Close() }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var got launchReceipt
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"refresh", "--background", "--index", path, "--transcripts", roots[0], "--transcripts", roots[1]}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("args=%q want %q", got.Args, want)
	}
	if got.Stdin != "/dev/null" || got.Stdout != "/dev/null" || got.Stderr != "/dev/null" {
		t.Fatalf("inherited stdio: %#v", got)
	}
}
