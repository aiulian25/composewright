package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseStats(t *testing.T) {
	services := map[string]string{"cw-x-app-1": "app", "cw-x-app-2": "app", "cw-x-db-1": "db", "cw-x-cache-1": "cache"}
	cases := []struct {
		name   string
		output string
		want   map[string]Usage
	}{
		{"binary units", `{"Name":"cw-x-app-1","MemUsage":"612MiB / 1GiB","CPUPerc":"12.50%"}`,
			map[string]Usage{"app": {MemoryBytes: 612 << 20, CPUPercent: 12.5}}},
		{"fractions and bytes", `{"Name":"cw-x-db-1","MemUsage":"1.5GiB / 2GiB","CPUPerc":"0.00%"}` + "\n" +
			`{"Name":"cw-x-cache-1","MemUsage":"0B / 0B","CPUPerc":"0.00%"}`,
			map[string]Usage{"db": {MemoryBytes: 3 << 29}, "cache": {}}},
		{"replicas keep the highest", `{"Name":"cw-x-app-1","MemUsage":"300MiB / 1GiB","CPUPerc":"5%"}` + "\n" +
			`{"Name":"cw-x-app-2","MemUsage":"100MiB / 1GiB","CPUPerc":"80%"}`,
			map[string]Usage{"app": {MemoryBytes: 300 << 20, CPUPercent: 80}}},
		{"unreadable and unknown lines are left out", "not json\n" + `{"Name":"cw-other-1","MemUsage":"1MiB / 1GiB","CPUPerc":"1%"}` + "\n" +
			`{"Name":"cw-x-db-1","MemUsage":"-- / --","CPUPerc":"--"}`, map[string]Usage{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := parseStats([]byte(testCase.output), services)
			if len(got) != len(testCase.want) {
				t.Fatalf("got %+v, want %+v", got, testCase.want)
			}
			for service, usage := range testCase.want {
				if got[service] != usage {
					t.Fatalf("%s: got %+v, want %+v", service, got[service], usage)
				}
			}
		})
	}
}

func TestValidateRecordsPeaks(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp + "\n" + healthyDB, stats: `{"Name":"cw-x-app-1","MemUsage":"612MiB / 2GiB","CPUPerc":"40%"}` + "\n" +
		`{"Name":"cw-x-db-1","MemUsage":"64MiB / 2GiB","CPUPerc":"3%"}`}
	verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")})
	if err != nil {
		t.Fatal(err)
	}
	app, db := verdict.Services[0], verdict.Services[1]
	if app.PeakMemoryBytes != 612<<20 || app.PeakCPUPercent != 40 || app.MemoryLimitBytes != 8<<30 || db.PeakMemoryBytes != 64<<20 {
		t.Fatalf("services %+v", verdict.Services)
	}
	if app.Peak() != "peak 612MiB of 8GiB, 40% CPU" || db.Peak() != "peak 64MiB, 3% CPU" {
		t.Fatalf("peaks %q %q", app.Peak(), db.Peak())
	}
}

func TestValidateKeepsPeakWhileStarting(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp, sampledWhileUp: make(chan struct{}),
		stats:        `{"Name":"cw-x-app-1","MemUsage":"900MiB / 2GiB","CPUPerc":"90%"}`,
		settledStats: `{"Name":"cw-x-app-1","MemUsage":"100MiB / 2GiB","CPUPerc":"1%"}`}
	sandbox := newTestSandbox(t, fake)
	sandbox.followInterval = 10 * time.Millisecond
	verdict, err := sandbox.Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")})
	if err != nil {
		t.Fatal(err)
	}
	if app := verdict.Services[0]; app.PeakMemoryBytes != 900<<20 || app.PeakCPUPercent != 90 {
		t.Fatalf("services %+v", verdict.Services)
	}
}

func TestValidatePassesWhenStatsFail(t *testing.T) {
	fake := &fakeDocker{ps: healthyApp, statsErr: errors.New("stats unavailable")}
	verdict, err := newTestSandbox(t, fake).Validate(context.Background(), Input{Project: "app", Compose: []byte(testCompose), Env: []byte("")})
	if err != nil || !verdict.Passed() || verdict.Services[0].PeakMemoryBytes != 0 || verdict.Services[0].Peak() != "" {
		t.Fatalf("verdict %+v, %v", verdict, err)
	}
}
