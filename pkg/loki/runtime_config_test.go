package loki

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/runtimeconfig"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/runtime"
	"github.com/grafana/loki/pkg/validation"
)

func Test_LoadRetentionRules(t *testing.T) {
	overrides := newTestOverrides(t,
		`
overrides:
    "1":
        creation_grace_period: 48h
    "29":
        creation_grace_period: 48h
        ingestion_burst_size_mb: 140
        ingestion_rate_mb: 120
        max_concurrent_tail_requests: 1000
        max_global_streams_per_user: 100000
        max_label_names_per_series: 30
        max_query_parallelism: 256
        split_queries_by_interval: 15m
        retention_period: 1440h
        retention_stream:
            - selector: '{app="foo"}'
              period: 48h
              priority: 10
            - selector: '{namespace="bar", cluster=~"fo.*|b.+|[1-2]"}'
              period: 24h
              priority: 5
`)
	require.Equal(t, time.Duration(0), overrides.RetentionPeriod("1"))   // default
	require.Equal(t, 2*30*24*time.Hour, overrides.RetentionPeriod("29")) // overrides
	require.Equal(t, []validation.StreamRetention(nil), overrides.StreamRetention("1"))
	require.Equal(t, []validation.StreamRetention{
		{Period: model.Duration(48 * time.Hour), Priority: 10, Selector: `{app="foo"}`, Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "app", "foo"),
		}},
		{Period: model.Duration(24 * time.Hour), Priority: 5, Selector: `{namespace="bar", cluster=~"fo.*|b.+|[1-2]"}`, Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "namespace", "bar"),
			labels.MustNewMatcher(labels.MatchRegexp, "cluster", "fo.*|b.+|[1-2]"),
		}},
	}, overrides.StreamRetention("29"))
}

func Test_ValidateRules(t *testing.T) {
	_, err := loadRuntimeConfig(strings.NewReader(
		`
overrides:
    "29":
        retention_stream:
            - selector: '{app=foo"}'
              period: 48h
              priority: 10
            - selector: '{namespace="bar", cluster=~"fo.*|b.+|[1-2]"}'
              period: 24h
              priority: 10
`))
	require.Equal(t, "invalid override for tenant 29: invalid labels matchers: parse error at line 1, col 6: syntax error: unexpected IDENTIFIER, expecting STRING", err.Error())
	_, err = loadRuntimeConfig(strings.NewReader(
		`
overrides:
    "29":
        retention_stream:
            - selector: '{app="foo"}'
              period: 5h
              priority: 10
`))
	require.Equal(t, "invalid override for tenant 29: retention period must be >= 24h was 5h", err.Error())
}

func Test_DefaultConfig(t *testing.T) {
	runtimeGetter := newTestRuntimeconfig(t,
		`
default:
    log_push_request: true
    limited_log_push_errors: false
configs:
    "1":
        log_push_request: false
        limited_log_push_errors: false
    "2":
        log_push_request: true
`)

	user1 := runtimeGetter("1")
	user2 := runtimeGetter("2")
	user3 := runtimeGetter("3")

	require.Equal(t, false, user1.LogPushRequest)
	require.Equal(t, false, user1.LimitedLogPushErrors)
	require.Equal(t, false, user2.LimitedLogPushErrors)
	require.Equal(t, true, user2.LogPushRequest)
	require.Equal(t, false, user3.LimitedLogPushErrors)
	require.Equal(t, true, user3.LogPushRequest)
}

func newTestRuntimeconfig(t *testing.T, yaml string) runtime.TenantConfig {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "bar")
	require.NoError(t, err)
	path := f.Name()
	// fake loader to load from string instead of file.
	loader := func(_ io.Reader) (interface{}, error) {
		return loadRuntimeConfig(strings.NewReader(yaml))
	}
	cfg := runtimeconfig.Config{
		ReloadPeriod: 1 * time.Second,
		Loader:       loader,
		LoadPath:     []string{path},
	}
	flagset := flag.NewFlagSet("", flag.PanicOnError)
	var defaults validation.Limits
	defaults.RegisterFlags(flagset)
	require.NoError(t, flagset.Parse(nil))

	reg := prometheus.NewPedanticRegistry()
	runtimeConfig, err := runtimeconfig.New(cfg, "test", prometheus.WrapRegistererWithPrefix("loki_", reg), log.NewNopLogger())
	require.NoError(t, err)

	require.NoError(t, runtimeConfig.StartAsync(context.Background()))
	require.NoError(t, runtimeConfig.AwaitRunning(context.Background()))
	defer func() {
		runtimeConfig.StopAsync()
		require.NoError(t, runtimeConfig.AwaitTerminated(context.Background()))
	}()

	return tenantConfigFromRuntimeConfig(runtimeConfig)
}

func newTestOverrides(t *testing.T, yaml string) *validation.Overrides {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "bar")
	require.NoError(t, err)
	path := f.Name()
	// fake loader to load from string instead of file.
	loader := func(_ io.Reader) (interface{}, error) {
		return loadRuntimeConfig(strings.NewReader(yaml))
	}
	cfg := runtimeconfig.Config{
		ReloadPeriod: 1 * time.Second,
		Loader:       loader,
		LoadPath:     []string{path},
	}
	flagset := flag.NewFlagSet("", flag.PanicOnError)
	var defaults validation.Limits
	defaults.RegisterFlags(flagset)
	require.NoError(t, flagset.Parse(nil))
	validation.SetDefaultLimitsForYAMLUnmarshalling(defaults)

	reg := prometheus.NewPedanticRegistry()
	runtimeConfig, err := runtimeconfig.New(cfg, "test", prometheus.WrapRegistererWithPrefix("loki_", reg), log.NewNopLogger())
	require.NoError(t, err)

	require.NoError(t, runtimeConfig.StartAsync(context.Background()))
	require.NoError(t, runtimeConfig.AwaitRunning(context.Background()))
	defer func() {
		runtimeConfig.StopAsync()
		require.NoError(t, runtimeConfig.AwaitTerminated(context.Background()))
	}()

	overrides, err := validation.NewOverrides(defaults, newtenantLimitsFromRuntimeConfig(runtimeConfig))
	require.NoError(t, err)
	return overrides
}

func Test_NoOverrides(t *testing.T) {
	flagset := flag.NewFlagSet("", flag.PanicOnError)

	var defaults validation.Limits
	defaults.RegisterFlags(flagset)
	require.NoError(t, flagset.Parse(nil))
	validation.SetDefaultLimitsForYAMLUnmarshalling(defaults)
	overrides, err := validation.NewOverrides(defaults, newtenantLimitsFromRuntimeConfig(nil))
	require.NoError(t, err)
	require.Equal(t, time.Duration(defaults.QuerySplitDuration), overrides.QuerySplitDuration("foo"))
}
