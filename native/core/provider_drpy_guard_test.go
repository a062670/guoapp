package core

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	drpyAllowPrivateTargets = true
	os.Exit(m.Run())
}

func TestDrpyCheckTargetRejectsLocalAddresses(t *testing.T) {
	drpyAllowPrivateTargets = false
	defer func() { drpyAllowPrivateTargets = true }()
	for _, raw := range []string{
		"http://127.0.0.1:8080/",
		"http://localhost/",
		"http://app.localhost/",
		"http://192.168.1.1/",
		"http://10.0.0.2/",
		"http://172.16.5.4/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://[fd00::1]/",
		"http://[::ffff:192.168.1.1]/",
	} {
		target, _ := url.Parse(raw)
		if err := drpyCheckTarget(context.Background(), target); !errors.Is(err, errDrpyPrivateTarget) {
			t.Errorf("%s: expected private target rejection, got %v", raw, err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/"} {
		target, _ := url.Parse(raw)
		if err := drpyCheckTarget(context.Background(), target); err == nil {
			t.Errorf("%s: expected scheme rejection", raw)
		}
	}
	for _, raw := range []string{"http://1.1.1.1/", "https://[2606:4700:4700::1111]/"} {
		target, _ := url.Parse(raw)
		if err := drpyCheckTarget(context.Background(), target); err != nil {
			t.Errorf("%s: expected public address to pass, got %v", raw, err)
		}
	}
}
