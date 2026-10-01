package host

import (
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform/linux"
	"github.com/modullar/blade-runner/internal/platform/macos"
)

func TestNewPicksThePlatformForTheOS(t *testing.T) {
	u := User{Name: "dev", UID: 501, Home: "/home/dev"}

	p, err := New("darwin", execx.OS{}, u)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := p.(*macos.Platform)
	if !ok || m.UID != 501 || m.Home != "/home/dev" || p.OS() != "darwin" {
		t.Errorf("darwin gave %T %+v", p, p)
	}

	p, err = New("linux", execx.OS{}, u)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := p.(*linux.Platform)
	if !ok || l.User != "dev" || l.Home != "/home/dev" || p.OS() != "linux" {
		t.Errorf("linux gave %T %+v", p, p)
	}
}

func TestUnsupportedOSIsRefusedWithACode(t *testing.T) {
	for _, goos := range []string{"windows", "freebsd", "plan9", ""} {
		if _, err := New(goos, execx.OS{}, User{}); diag.CodeOf(err) != diag.CodeUnsupportedPlatform {
			t.Errorf("%q: err = %v, want BR-E011", goos, err)
		}
	}
}
