package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// Testing the runner needs a stand-in for an agent CLI: something that prints
// lines, goes quiet, exits non-zero, floods stderr, or leaves a child behind.
//
// A shell script is why these tests used to be unix-only. Instead the test
// binary re-executes itself in helper mode, as the os/exec tests do, which
// makes them portable — including the process-tree test, on the platform where
// it matters most.
const helperEnv = "AGENT2API_RUNNER_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain(os.Args[1:])
		return
	}
	os.Exit(m.Run())
}

// helperMain is the stand-in CLI. It reports usage errors on stderr and exits
// 2, so a mistyped test reads as a broken helper rather than as a runner bug.
func helperMain(args []string) {
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "helper: "+format+"\n", a...)
		os.Exit(2)
	}
	if len(args) == 0 {
		fail("no command given")
	}

	switch args[0] {
	case "emit": // emit LINE...  — one line each, then exit
		for _, line := range args[1:] {
			fmt.Println(line)
		}

	case "cat": // cat — copy stdin to stdout
		if _, err := os.Stdout.ReadFrom(os.Stdin); err != nil {
			fail("reading stdin: %v", err)
		}

	case "fail": // fail CODE MESSAGE — message on stderr, exit CODE
		if len(args) != 3 {
			fail("fail needs a code and a message")
		}
		code, err := strconv.Atoi(args[1])
		if err != nil {
			fail("bad exit code %q", args[1])
		}
		fmt.Fprintln(os.Stderr, args[2])
		os.Exit(code)

	case "sleep": // sleep DURATION — say nothing for that long
		time.Sleep(mustDuration(fail, args, 1))

	case "emit-then-sleep": // emit-then-sleep LINE DURATION — one line, then quiet
		if len(args) != 3 {
			fail("emit-then-sleep needs a line and a duration")
		}
		fmt.Println(args[1])
		time.Sleep(mustDuration(fail, args, 2))

	case "tick": // tick COUNT INTERVAL — keep talking, to stay under an idle limit
		if len(args) != 3 {
			fail("tick needs a count and an interval")
		}
		count, err := strconv.Atoi(args[1])
		if err != nil {
			fail("bad count %q", args[1])
		}
		every := mustDuration(fail, args, 2)
		for i := 0; i < count; i++ {
			fmt.Println("tick")
			time.Sleep(every)
		}

	case "flood": // flood COUNT — many lines, then stay alive
		count, err := strconv.Atoi(args[1])
		if err != nil {
			fail("bad count %q", args[1])
		}
		for i := 0; i < count; i++ {
			fmt.Println("line")
		}
		time.Sleep(30 * time.Second)

	case "noise": // noise COUNT — many stderr lines, then exit 1
		count, err := strconv.Atoi(args[1])
		if err != nil {
			fail("bad count %q", args[1])
		}
		for i := 1; i <= count; i++ {
			fmt.Fprintf(os.Stderr, "noise line %d\n", i)
		}
		os.Exit(1)

	case "pwd": // pwd — print the working directory
		dir, err := os.Getwd()
		if err != nil {
			fail("getwd: %v", err)
		}
		fmt.Println(dir)

	case "spawn-child": // spawn-child MARKER — start a heartbeat that outlives us
		if len(args) != 2 {
			fail("spawn-child needs a marker path")
		}
		child := exec.Command(os.Args[0], "heartbeat", args[1])
		child.Env = append(os.Environ(), helperEnv+"=1")
		if err := child.Start(); err != nil {
			fail("cannot spawn the child: %v", err)
		}
		fmt.Println("started")
		time.Sleep(30 * time.Second)

	case "heartbeat": // heartbeat MARKER — append to MARKER until killed
		if len(args) != 2 {
			fail("heartbeat needs a marker path")
		}
		for {
			f, err := os.OpenFile(args[1], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, time.Now().UnixNano())
				f.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}

	default:
		fail("unknown command %q", args[0])
	}
}

func mustDuration(fail func(string, ...any), args []string, i int) time.Duration {
	if i >= len(args) {
		fail("%s needs a duration", args[0])
	}
	d, err := time.ParseDuration(args[i])
	if err != nil {
		fail("bad duration %q", args[i])
	}
	return d
}

// helper builds a Spec that runs the stand-in CLI with the given arguments.
func helper(t *testing.T, args ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary to re-execute: %v", err)
	}
	return Spec{
		Binary: exe,
		Args:   args,
		// The allowlist would drop the marker that puts the binary in helper
		// mode, so it goes through as an explicit extra.
		Env:            Environ(nil, nil, map[string]string{helperEnv: "1"}),
		RequestTimeout: 30 * time.Second,
	}
}
