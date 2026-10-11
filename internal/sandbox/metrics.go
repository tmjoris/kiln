package sandbox

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
)

var startLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "kiln_sandbox_start_seconds",
	Help:    "Time from CreateSandbox to a ready, personalised guest.",
	Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
}, []string{"backend", "mode"})

func observeStart(b kilnv1.Backend, mode kilnv1.StartMode, d time.Duration) {
	startLatency.WithLabelValues(backendName(b), modeName(mode)).Observe(d.Seconds())
}

func modeName(m kilnv1.StartMode) string {
	switch m {
	case kilnv1.StartMode_START_MODE_COLD_BOOT:
		return "cold_boot"
	case kilnv1.StartMode_START_MODE_SNAPSHOT_RESTORE:
		return "snapshot_restore"
	case kilnv1.StartMode_START_MODE_WARM_POOL:
		return "warm_pool"
	}
	return "unknown"
}
