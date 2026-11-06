package integration

// End-to-end tests for the output-channel model of `list` and `show`
// (docs/requirements.md 3.6) and for the two guards that protect it: the
// same-path refusal and --no-overwrite. Every cell of the matrix runs the shipped
// binary, because the contract is about which file descriptor receives which
// bytes. Skipped unless IMEX_TEST_DSN is set.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// channelCase describes one successful result together with a failing run of the
// same command, so the four cells and the no-partial-result rule can be asserted
// the same way for both commands.
type channelCase struct {
	// slug names the files this case writes.
	slug string
	// run invokes the command with the given flags appended.
	run func(args ...string) (stdout, stderr string, code int)
	// fail invokes the same command so the server reports an error: a missing
	// object for show, an unusable flag value for list.
	fail func(args ...string) (stdout, stderr string, code int)
	// expect is a string only the result bytes contain.
	expect string
}

func TestCLIChannelMatrix(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)
	gone := fmt.Sprintf("imex_no_such_%d", time.Now().UnixNano())

	cases := []channelCase{
		{
			slug: "list",
			run:  func(a ...string) (string, string, int) { return env.runListCLI(t, append([]string{"tables"}, a...)...) },
			fail: func(a ...string) (string, string, int) {
				return env.runListCLI(t, append([]string{"tables", "--type", "nonsense"}, a...)...)
			},
			expect: fix.table,
		},
		{
			slug: "show",
			run: func(a ...string) (string, string, int) {
				return env.runShowCLI(t, append([]string{"tab", fix.table}, a...)...)
			},
			fail: func(a ...string) (string, string, int) {
				return env.runShowCLI(t, append([]string{"tab", gone}, a...)...)
			},
			// A marker only the definition body carries: the log records echo the SHOW
			// statement, which names the object, so the name alone cannot tell a
			// leaked result from an ordinary interaction record.
			expect: "ENGINE=InnoDB DEFAULT CHARSET",
		},
	}

	for _, cc := range cases {
		t.Run(cc.slug, func(t *testing.T) {
			// withInfo raises the log level for one call: the records the cells
			// below point at are written at info, and the shared base flags ask for
			// error. It builds a fresh slice so no call can share a backing array.
			withInfo := func(extra ...string) []string {
				return append([]string{"--log-level", "info"}, extra...)
			}

			// Cell 1: --output alone. Result in the file, logs still on the screen,
			// stdout empty.
			t.Run("output only", func(t *testing.T) {
				path := filepath.Join(env.dir, cc.slug+"-1.txt")
				stdout, stderr, code := cc.run(withInfo("-o", path)...)
				if code != 0 {
					t.Fatalf("exit %d, stderr:\n%s", code, stderr)
				}
				if stdout != "" {
					t.Errorf("stdout must be empty with --output, got:\n%s", stdout)
				}
				assertResultFile(t, path, cc.expect)
				if !strings.Contains(stderr, "imex starting") {
					t.Errorf("logs must stay on the screen without --messages:\n%s", stderr)
				}
			})

			// Cell 2: --output and --messages. Result in the file, logs in the
			// message file, nothing on the screen.
			t.Run("output and messages", func(t *testing.T) {
				path := filepath.Join(env.dir, cc.slug+"-2.txt")
				msg := filepath.Join(env.dir, cc.slug+"-2.log")
				stdout, stderr, code := cc.run(withInfo("-o", path, "-m", msg)...)
				if code != 0 {
					t.Fatalf("exit %d, stderr:\n%s", code, stderr)
				}
				if stdout != "" || stderr != "" {
					t.Errorf("--messages must silence the screen, stdout=%q stderr=%q", stdout, stderr)
				}
				assertResultFile(t, path, cc.expect)
				logText := readFile(t, msg)
				if !strings.Contains(logText, "imex starting") {
					t.Errorf("message file missing the log records:\n%s", logText)
				}
				if strings.Contains(logText, cc.expect) {
					t.Errorf("with --output the result must not also land in the message file:\n%s", logText)
				}
			})

			// Cell 3: neither flag. Result on stdout, logs on stderr.
			t.Run("no result flags", func(t *testing.T) {
				stdout, stderr, code := cc.run(withInfo()...)
				if code != 0 {
					t.Fatalf("exit %d, stderr:\n%s", code, stderr)
				}
				if !strings.Contains(stdout, cc.expect) {
					t.Errorf("stdout missing the result:\n%s", stdout)
				}
				if strings.Contains(stdout, "imex starting") {
					t.Errorf("logs leaked into stdout:\n%s", stdout)
				}
				if !strings.Contains(stderr, "imex starting") {
					t.Errorf("logs must be on the screen:\n%s", stderr)
				}
			})

			// Cell 4: --messages alone. Log and result share one file, the screen is
			// silent, and the file reads as log first, result last.
			t.Run("messages only", func(t *testing.T) {
				msg := filepath.Join(env.dir, cc.slug+"-4.log")
				stdout, stderr, code := cc.run(withInfo("-m", msg)...)
				if code != 0 {
					t.Fatalf("exit %d, stderr:\n%s", code, stderr)
				}
				if stdout != "" || stderr != "" {
					t.Errorf("--messages must silence the screen, stdout=%q stderr=%q", stdout, stderr)
				}
				logText := readFile(t, msg)
				idx := strings.Index(logText, cc.expect)
				if idx < 0 {
					t.Fatalf("message file missing the appended result:\n%s", logText)
				}
				done := strings.Index(logText, "completed")
				if done < 0 {
					t.Fatalf("message file missing the completion record:\n%s", logText)
				}
				if done > idx {
					t.Errorf("the completion record must precede the result:\n%s", logText)
				}
				// Appending never truncates: the run's own banner still opens the file.
				if !strings.HasPrefix(logText, "20") || !strings.Contains(logText, "imex starting") {
					t.Errorf("message file does not start with the log banner:\n%s", logText)
				}
			})

			// A failing run delivers nothing: the result is buffered and only handed
			// to its channel after the command succeeded.
			t.Run("failure leaves no result", func(t *testing.T) {
				path := filepath.Join(env.dir, cc.slug+"-fail.txt")
				stdout, stderr, code := cc.fail("-o", path)
				if code == 0 {
					t.Fatalf("the failing invocation unexpectedly succeeded:\n%s", stderr)
				}
				if stdout != "" {
					t.Errorf("stdout must be empty on failure:\n%s", stdout)
				}
				if _, err := os.Stat(path); err == nil {
					t.Errorf("--output must not be created for the run that failed:\n%s", readFile(t, path))
				}
			})
		})
	}
}

// TestCLIResultAndMessageSameFileIsRefused covers the guard on all four commands:
// one path for the result and the message file is a usage error (exit 2), and it
// never silently truncates a log that is already open.
func TestCLIResultAndMessageSameFileIsRefused(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)

	cases := []struct {
		name      string
		resultArg string // the flag whose path collides with --messages
		optional  bool   // list and show can drop it; export and import cannot
		args      func(path string) []string
	}{
		{
			name: "list", resultArg: "--output", optional: true,
			args: func(p string) []string {
				return append(append([]string{"list", "tables"}, env.base...), "-o", p, "-m", p)
			},
		},
		{
			name: "show", resultArg: "--output", optional: true,
			args: func(p string) []string {
				return append(append([]string{"show", "tab", fix.table}, env.base...), "-o", p, "-m", p)
			},
		},
		{
			name: "export", resultArg: "--output", optional: false,
			args: func(p string) []string {
				return append(append([]string{"export", "-q", "SELECT 1", "-o", p}, env.base...), "-m", p)
			},
		},
		{
			name: "import", resultArg: "--input", optional: false,
			args: func(p string) []string {
				// --table is always required now, so supply it; the run then reaches
				// the path guard (--input and --messages on the same file) instead of
				// the missing-table check.
				return append(append([]string{"import", "-f", "sql", "-t", fix.table, "-i", p}, env.base...), "-m", p)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(env.dir, tc.name+"-same.log")
			stdout, stderr, code := runIMEXSplit(t, tc.args(path))
			if code != 2 {
				t.Fatalf("exit = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout must be empty:\n%s", stdout)
			}
			// --messages opens the log before validation runs, so the refusal is
			// recorded there and the screen shows it no longer.
			if stderr != "" {
				t.Errorf("with --messages the screen must stay silent, got:\n%s", stderr)
			}
			text := readFile(t, path)
			if !strings.Contains(text, tc.resultArg+" and --messages must name different files") {
				t.Errorf("message file missing the refusal:\n%s", text)
			}
			escape := "omit " + tc.resultArg
			if tc.optional {
				if !strings.Contains(text, escape) {
					t.Errorf("an optional result flag must name the way out:\n%s", text)
				}
			} else if strings.Contains(text, "omit") {
				t.Errorf("a required result flag must not suggest omitting it:\n%s", text)
			}
		})
	}
}

// TestCLINoOverwriteOnListAndShow pins the collision refusal: exit 1 (a runtime
// decision, the file is not the user's intent), export's exact wording, and the
// existing file untouched. Default behaviour stays truncate, as export already did.
func TestCLINoOverwriteOnListAndShow(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)
	const stale = "bytes that must survive\n"

	cases := []struct {
		slug string
		args func(path string, noOverwrite bool) []string
	}{
		{"list", func(p string, no bool) []string {
			out := append([]string{"list", "tables"}, env.base...)
			out = append(out, "-o", p)
			if no {
				out = append(out, "--no-overwrite")
			}
			return out
		}},
		{"show", func(p string, no bool) []string {
			out := append([]string{"show", "tab", fix.table}, env.base...)
			out = append(out, "-o", p)
			if no {
				out = append(out, "--no-overwrite")
			}
			return out
		}},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			path := filepath.Join(env.dir, tc.slug+"-overwrite.txt")

			if err := os.WriteFile(path, []byte(stale), 0666); err != nil {
				t.Fatal(err)
			}
			if _, stderr, code := runIMEXSplit(t, tc.args(path, false)); code != 0 {
				t.Fatalf("default overwrite exit %d, stderr:\n%s", code, stderr)
			}
			if got := readFile(t, path); strings.HasPrefix(got, stale) {
				t.Errorf("--output must truncate the existing file:\n%s", got)
			}

			if err := os.WriteFile(path, []byte(stale), 0666); err != nil {
				t.Fatal(err)
			}
			_, stderr, code := runIMEXSplit(t, tc.args(path, true))
			if code != 1 {
				t.Fatalf("--no-overwrite exit = %d, want 1 (a runtime refusal), stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "already exists, remove --no-overwrite to overwrite") {
				t.Errorf("stderr does not carry export's wording:\n%s", stderr)
			}
			if got := readFile(t, path); got != stale {
				t.Errorf("existing file changed:\n%s", got)
			}
		})
	}
}

// TestCLIMessagesStaysAppendOnlyAcrossRuns pins the other half of the --messages
// ruling: a second run into the same message file keeps the first run's bytes.
func TestCLIMessagesStaysAppendOnlyAcrossRuns(t *testing.T) {
	env := newCLILive(t)
	fix := newShowFixtures(t, env)
	msg := filepath.Join(env.dir, "append.log")

	for i := 0; i < 2; i++ {
		if _, stderr, code := env.runShowCLI(t, "tab", fix.table, "-m", msg); code != 0 {
			t.Fatalf("run %d exit %d, stderr:\n%s", i, code, stderr)
		}
	}
	text := readFile(t, msg)
	if got := strings.Count(text, "CREATE TABLE `"+fix.table+"`"); got != 2 {
		t.Errorf("appended results = %d, want 2:\n%s", got, text)
	}
	if got := strings.Count(text, "imex starting"); got != 2 {
		t.Errorf("run banners = %d, want 2 (the file was never truncated):\n%s", got, text)
	}
}

// assertResultFile checks a --output file holds the result and nothing else: no
// log records, no banner.
func assertResultFile(t *testing.T, path, expect string) {
	t.Helper()
	text := readFile(t, path)
	if !strings.Contains(text, expect) {
		t.Errorf("%s missing %q:\n%s", path, expect, text)
	}
	for _, leak := range []string{"imex starting", "db interaction", "completed"} {
		if strings.Contains(text, leak) {
			t.Errorf("log record %q leaked into the result file %s:\n%s", leak, path, text)
		}
	}
}
