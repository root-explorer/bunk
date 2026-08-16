package cli

import (
	"strings"
	"testing"

	"bunk/internal/daemon"
)

func cfg() daemon.Config {
	c := daemon.DefaultConfig()
	c.Limits.CPUs = 6
	c.Limits.MemoryGB = 12
	c.Limits.Pids = 256
	return c
}

func TestInjectDefaultsAddsLimits(t *testing.T) {
	args := []string{"run", "postgres:16"}
	has, _, _ := scanRunArgs(args[1:])
	got := injectDefaults(args, has, cfg(), "none", 0, 0)
	want := "docker run --cpus 6 --memory 12g --pids-limit 256 postgres:16"
	if strings.Join(got, " ") != strings.TrimPrefix(want, "docker ") {
		t.Fatalf("got %q want %q", strings.Join(got, " "), want)
	}
}

func TestInjectDefaultsRespectsUserFlags(t *testing.T) {
	args := []string{"run", "--cpus", "2", "--memory", "1g", "-d", "redis:7"}
	has, _, _ := scanRunArgs(args[1:])
	got := injectDefaults(args, has, cfg(), "none", 0, 0)
	for _, f := range []string{"--cpus", "2", "--memory", "1g"} {
		if !strings.Contains(strings.Join(got, " "), f) {
			t.Fatalf("missing user flag %s in %v", f, got)
		}
	}
	if !strings.Contains(strings.Join(got, " "), "--pids-limit") {
		t.Fatalf("pids-limit should be injected when absent: %v", got)
	}
	if !strings.Contains(strings.Join(got, " "), "redis:7") {
		t.Fatalf("image lost: %v", got)
	}
}

func TestInjectGPUOnlyForNvidia(t *testing.T) {
	has := map[string]bool{}
	args := []string{"run", "nvidia/cuda:12.0"}
	got := injectDefaults(args, has, cfg(), "nvidia", 0, 0)
	if !strings.Contains(strings.Join(got, " "), "--gpus all") {
		t.Fatalf("expected --gpus all on nvidia host: %v", got)
	}
	got = injectDefaults(args, has, cfg(), "none", 0, 0)
	if strings.Contains(strings.Join(got, " "), "--gpus") {
		t.Fatalf("no --gpus expected on non-nvidia host: %v", got)
	}
	off := cfg()
	off.GPU = "off"
	got = injectDefaults(args, has, off, "nvidia", 0, 0)
	if strings.Contains(strings.Join(got, " "), "--gpus") {
		t.Fatalf("gpu off must disable injection: %v", got)
	}
}

func TestScanStopsAtImage(t *testing.T) {
	// --cpus after the image is a container arg, not a docker flag.
	args := []string{"run", "--rm", "alpine", "sh", "-c", "--cpus", "999"}
	has, _, _ := scanRunArgs(args[1:])
	if has["--cpus"] {
		t.Fatalf("--cpus after image must not be treated as a docker flag")
	}
	if !has["--rm"] {
		t.Fatalf("--rm before image must be seen")
	}
}

func TestScanPublishes(t *testing.T) {
	_, pubs, _ := scanRunArgs([]string{"-p", "5432:5432", "-p8080:80", "--publish=9000:9000", "-d", "postgres"})
	if len(pubs) != 3 {
		t.Fatalf("expected 3 publishes, got %v", pubs)
	}
	if pubs[0] != "5432:5432" || pubs[1] != "8080:80" || pubs[2] != "9000:9000" {
		t.Fatalf("publish parsing wrong: %v", pubs)
	}
}

func TestScanDetached(t *testing.T) {
	_, _, detached := scanRunArgs([]string{"-d", "postgres"})
	if !detached {
		t.Fatal("expected detached=true for -d")
	}
	_, _, detached = scanRunArgs([]string{"--detach", "postgres"})
	if !detached {
		t.Fatal("expected detached=true for --detach")
	}
	_, _, detached = scanRunArgs([]string{"postgres"})
	if detached {
		t.Fatal("expected detached=false")
	}
}

func TestParsePublishHostPort(t *testing.T) {
	for spec, want := range map[string]int{
		"5432:5432":         5432,
		"127.0.0.1:8080:80": 8080,
	} {
		got, ok := parsePublishHostPort(spec)
		if !ok || got != want {
			t.Fatalf("%s: got %d ok=%v want %d", spec, got, ok, want)
		}
	}
	if _, ok := parsePublishHostPort("5432"); ok {
		t.Fatal("bare container port should not auto-forward (random host port)")
	}
}

func TestCombinedShortFlags(t *testing.T) {
	has, pubs, detached := scanRunArgs([]string{"-dit", "-p5432:5432", "postgres"})
	if !detached {
		t.Fatal("expected -d in -dit")
	}
	if !has["-i"] || !has["-t"] {
		t.Fatal("combined -it not parsed")
	}
	if len(pubs) != 1 || pubs[0] != "5432:5432" {
		t.Fatalf("attached -p value not parsed: %v", pubs)
	}
}

func TestInjectDefaultsClampsToHostCapacity(t *testing.T) {
	// Small host (2 cores, 2 GB): defaults clamp down.
	args := []string{"run", "postgres:16"}
	has, _, _ := scanRunArgs(args[1:])
	got := injectDefaults(args, has, cfg(), "none", 2, 2)
	s := strings.Join(got, " ")
	for _, want := range []string{"--cpus 2", "--memory 1g", "--pids-limit 256"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing clamped %s in %q", want, s)
		}
	}
	// Big host (16 cores, 32 GB): defaults unchanged.
	got = injectDefaults(args, has, cfg(), "none", 16, 32)
	s = strings.Join(got, " ")
	for _, want := range []string{"--cpus 6", "--memory 12g"} {
		if !strings.Contains(s, want) {
			t.Fatalf("defaults should survive on big hosts: %q", s)
		}
	}
	// Unknown caps (0,0): defaults unchanged (old state).
	got = injectDefaults(args, has, cfg(), "none", 0, 0)
	s = strings.Join(got, " ")
	if !strings.Contains(s, "--cpus 6") || !strings.Contains(s, "--memory 12g") {
		t.Fatalf("defaults should survive with unknown caps: %q", s)
	}
}
