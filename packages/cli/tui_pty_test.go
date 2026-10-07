//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mxiao/todo-cli/packages/core"
)

// TestMain lets the PTY tests re-execute this test binary as the `todo`
// command: with TODO_CLI_E2E_CHILD=1 it behaves exactly like cmd/todo.
func TestMain(m *testing.M) {
	if os.Getenv("TODO_CLI_E2E_CHILD") == "1" {
		os.Exit(Run(os.Args[1:], OSEnv()))
	}
	os.Exit(m.Run())
}

// ptyProc is `todo` running on a pseudo-terminal, like a real Terminal.app
// or iTerm2 session: input is raw key and mouse escape sequences.
type ptyProc struct {
	t      *testing.T
	master *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	out    []byte
	done   chan error
}

func startPTY(t *testing.T, cols, rows int, args ...string) *ptyProc {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal support: %v", err)
	}
	name, err := ptsName(fd)
	if err != nil {
		unix.Close(fd)
		t.Skipf("pty setup: %v", err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	slave, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("open %s: %v", name, err)
	}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "TODO_CLI_E2E_CHILD=1", "TERM=xterm-256color", "NO_COLOR=", "TODO_CLI_MOUSE=", "TODO_CLI_KEYMAP=")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	p := &ptyProc{t: t, master: master, cmd: cmd, done: make(chan error, 1)}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			p.mu.Lock()
			p.out = append(p.out, buf[:n]...)
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		master.Close()
	})
	return p
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?<]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]`)

// text is everything printed so far, with escape sequences removed.
func (p *ptyProc) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ansi.ReplaceAllString(string(p.out), "")
}

func (p *ptyProc) raw() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.out)
}

func (p *ptyProc) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.out)
}

// waitFor waits until want appears in the output printed after mark.
func (p *ptyProc) waitFor(mark int, want string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := ansi.ReplaceAllString(string(p.out[min(mark, len(p.out)):]), "")
		p.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %q; screen output:\n%s", want, p.text())
}

// send writes input and, so that a following ESC is not merged into an
// Alt+key sequence, pauses briefly like a human typist.
func (p *ptyProc) send(s string) {
	p.t.Helper()
	if _, err := p.master.Write([]byte(s)); err != nil {
		p.t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
}

// do sends input and waits for the response text.
func (p *ptyProc) do(input, want string) {
	p.t.Helper()
	m := p.mark()
	p.send(input)
	p.waitFor(m, want)
}

func (p *ptyProc) wait() {
	p.t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			p.t.Fatalf("todo exited with %v; output:\n%s", err, p.text())
		}
	case <-time.After(10 * time.Second):
		p.t.Fatalf("todo did not exit; output:\n%s", p.text())
	}
}

func click(button, col, row int) string {
	return fmt.Sprintf("\x1b[<%d;%d;%dM\x1b[<%d;%d;%dm", button, col, row, button, col, row)
}

func seed(t *testing.T, dir string, titles ...string) {
	t.Helper()
	s, err := core.Open(core.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, title := range titles {
		if _, err := s.Create(core.NewTask{Title: title}); err != nil {
			t.Fatal(err)
		}
	}
}

func loadAll(t *testing.T, dir string) map[string]core.Task {
	t.Helper()
	s, err := core.Open(core.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all, err := s.List(core.Filter{IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]core.Task{}
	for _, tk := range all {
		out[tk.Title] = tk
	}
	return out
}

func TestTUIKeyboardAndMouseOverPTY(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "第一项", "第二项")
	p := startPTY(t, 100, 30, "--data-dir", dir, "tui")
	p.waitFor(0, "第二项")
	if raw := p.raw(); !strings.Contains(raw, "\x1b[?1049h") || !strings.Contains(raw, "\x1b[?1000h\x1b[?1006h") {
		t.Errorf("alternate screen and SGR mouse reporting should be enabled")
	}

	// Keyboard: quick add with metadata.
	p.do("a", "新任务")
	p.do("写周报 #work !high\r", "已创建：写周报")

	// Mouse: click the first row (1-based row 3), complete it with the keyboard.
	p.send(click(0, 10, 3))
	p.do("x", "已完成：第一项")

	// Keyboard continues from the clicked row: j selects 第二项, s starts it.
	p.do("js", "已开始：第二项")

	// Right click on row 2 opens the context menu at the pointer (0-based
	// x=11, y=3). Its items start one row lower; "删除" is the 9th item.
	p.do(click(2, 12, 4), "打开详情")
	p.do(click(0, 15, 13), "确定删除「第二项」")
	p.do("y", "已删除：第二项")

	// Wheel events scroll without breaking anything.
	p.send("\x1b[<65;10;5M\x1b[<64;10;5M")

	// Search, help and the command palette.
	p.do("/写周\r", "搜索「写周」")
	p.send("\x1b") // clear the search
	p.do("?", "快捷键帮助")
	p.send("\x1b")
	p.do(":", "命令面板")
	p.do("filter status done\r", "筛选：状态 已完成")
	p.do("1", "筛选：全部")

	// Edit form: replace the title of the selected task and save with ctrl+s.
	p.do("ge", "编辑任务")
	p.do("\x15第一项（改）\x13", "已保存：第一项（改）")

	p.send("q")
	p.wait()
	if raw := p.raw(); !strings.HasSuffix(strings.TrimRight(raw, "\r\n"), "\x1b[?1049l") || !strings.Contains(raw, "\x1b[?1000l") {
		t.Errorf("terminal modes must be restored on exit: %q", raw[max(len(raw)-80, 0):])
	}

	tasks := loadAll(t, dir)
	if tk := tasks["第一项（改）"]; tk.Status != core.StatusDone {
		t.Errorf("第一项 should be done and renamed: %+v", tk)
	}
	if tk := tasks["第二项"]; tk.DeletedAt == nil || tk.Status != core.StatusInProgress {
		t.Errorf("第二项 should be started then deleted: %+v", tk)
	}
	if tk := tasks["写周报"]; tk.Priority != core.PriorityHigh || strings.Join(tk.Tags, ",") != "work" {
		t.Errorf("写周报: %+v", tk)
	}
	s, _ := core.Open(core.Options{DataDir: dir})
	defer s.Close()
	hist, _ := s.History(tasks["写周报"].ID)
	if len(hist) == 0 || hist[0].Actor != "tui" {
		t.Errorf("changes made in the TUI are recorded with actor tui: %+v", hist)
	}
}

func TestTUIKeyboardOnlyOverPTY(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "仅键盘")
	// Bare `todo` in a terminal starts the interactive UI.
	p := startPTY(t, 80, 24, "--data-dir", dir)
	p.waitFor(0, "仅键盘")
	p.send("q")
	p.wait()

	p = startPTY(t, 80, 24, "--data-dir", dir, "tui", "--no-mouse")
	p.waitFor(0, "仅键盘")
	if strings.Contains(p.raw(), "\x1b[?1000h") {
		t.Errorf("--no-mouse must not enable mouse reporting")
	}
	// Narrow terminal: enter opens the detail view, tab returns, m opens the menu.
	p.do("\r", "详情 · tab")
	p.do("x", "已完成：仅键盘")
	p.send("\t")
	p.do("m", "重新打开")
	p.send("\x1b")
	p.do("u", "已撤销")
	p.send("\x03") // ctrl+c quits
	p.wait()
	if tk := loadAll(t, dir)["仅键盘"]; tk.Status != core.StatusTodo {
		t.Errorf("undo should have reopened the task: %v", tk.Status)
	}
}

func TestTUICustomKeymapOverPTY(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "自定义")
	if err := os.WriteFile(dir+"/keybindings.json", []byte(`{"toggle_done": "D", "quit": ["Q", "ctrl+c"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startPTY(t, 100, 30, "--data-dir", dir, "tui")
	p.waitFor(0, "D 完成")
	p.do("x", "按键 x 未绑定任何操作")
	p.do("D", "已完成：自定义")
	p.send("q") // q is no longer bound
	select {
	case <-p.done:
		t.Fatal("q should not quit after rebinding quit to Q")
	case <-time.After(200 * time.Millisecond):
	}
	p.send("Q")
	p.wait()
}

func TestTUIRequiresTerminal(t *testing.T) {
	h := newHarness(t)
	_, stderr, code := h.run("tui")
	if code != ExitUsage || !strings.Contains(stderr, "needs a terminal") {
		t.Errorf("tui without a terminal: %d %s", code, stderr)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "--data-dir", t.TempDir())
	cmd.Env = append(os.Environ(), "TODO_CLI_E2E_CHILD=1")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "commands:") {
		t.Errorf("bare todo without a terminal prints help: %s", out)
	}
}
