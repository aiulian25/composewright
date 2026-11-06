package config

import (
	"testing"
	"time"
)

func TestNetworkBudgetsAreFiniteAndConfigurable(t *testing.T) {
	valid := Default()
	valid.Network.ProviderTimeout = 45 * time.Minute
	valid.Network.ProviderIdleTimeout = 15 * time.Minute
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, duration := range []time.Duration{0, -time.Second, 25 * time.Hour} {
		bad := Default()
		bad.Network.IntakeTimeout = duration
		if bad.Validate() == nil {
			t.Fatalf("accepted unbounded/invalid budget %s", duration)
		}
	}
}
