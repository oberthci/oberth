package gitcache_test

import (
	"context"
	"encoding/json"

	"os"
	"path/filepath"
	"strconv"
	"strings"

	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func reviewConfig(t *testing.T, f *gittest.Fixture) gitcache.Config {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return gitcache.Config{
		Root: filepath.Join(f.Root, "cache"), Scratch: filepath.Join(f.Root, "scratch"),
		Binary: f.Binary, HookBinary: fastHook(t, bin),
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{URL: f.Bare, DefaultBranch: "refs/heads/main", RepositoryID: 1}, nil
		},
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil }}
}
func reviewNew(t *testing.T, cfg gitcache.Config) *gitcache.Cache {
	t.Helper()
	c, e := gitcache.New(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func reviewWrapper(t *testing.T, f *gittest.Fixture) string {
	t.Helper()
	script := filepath.Join(f.Root, "git-wrapper")
	shim := filepath.Join(f.Root, "fault.so")
	image, err := nativeFaultImage()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, image, 0700); err != nil {
		t.Fatal(err)
	}
	body := `#!/usr/bin/python3
import os,sys,json,signal
root=os.path.dirname(__file__)
fd=os.open(root+'/commands.jsonl',os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
os.write(fd,(json.dumps({'pid':os.getpid(),'argv':sys.argv[1:]})+'\n').encode());os.close(fd)
try:
 cfg=json.load(open(root+'/fault.json'))
except FileNotFoundError:
 cfg={}
if all(x in sys.argv[1:] for x in cfg.get('args', ['NEVER'])):
 if cfg.get('stop'):
  open(root+'/stopped','w').write(str(os.getpid()))
  os.kill(os.getpid(),signal.SIGSTOP)
 else:
  os.environ.update(LD_PRELOAD='` + shim + `',
   REVIEW_NEEDLE=cfg['needle'],REVIEW_PHASE=cfg['phase'],
   REVIEW_MARKER=root+'/stopped')
os.execv('/usr/bin/git',['git']+sys.argv[1:])
`
	if e := os.WriteFile(script, []byte(body), 0700); e != nil {
		t.Fatal(e)
	}
	return script
}
func reviewFault(t *testing.T, f *gittest.Fixture, v map[string]any) {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(f.Root, "fault.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func reviewWait(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		b, e := os.ReadFile(path)
		if e == nil && len(b) > 0 {
			return b
		}
		select {
		case <-deadline:
			t.Fatal("fault point not reached", path)
		case <-tick.C:
		}
	}
}
func reviewKill(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	b := reviewWait(t, filepath.Join(f.Root, "stopped"))
	pid, e := strconv.Atoi(strings.Split(string(b), "\n")[0])
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Kill(pid, syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	t.Logf("killed own Git PID %d at %s", pid, strings.ReplaceAll(string(b), "\n", " | "))
}
func reviewDisarm(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	if e := os.Remove(filepath.Join(f.Root, "fault.json")); e != nil {
		t.Fatal(e)
	}
}
func reviewLocks(t *testing.T, f *gittest.Fixture) []string {
	t.Helper()
	var out []string
	e := filepath.WalkDir(f.Store(), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if strings.HasSuffix(p, ".lock") {
			out = append(out, p)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func fastHook(t *testing.T, binary string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook")
	body := "#!/usr/bin/python3\nimport os,sys\n" +
		"os.environ['GORACE']='atexit_sleep_ms=0'\nos.execve(" +
		strconv.Quote(binary) + ",sys.argv,os.environ)\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
