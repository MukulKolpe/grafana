package ngalert

import (
	"context"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/grafana/grafana/pkg/services/quota"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/grafana/pkg/services/ngalert/api"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	"github.com/grafana/grafana/pkg/services/ngalert/metrics"
	"github.com/grafana/grafana/pkg/services/ngalert/state"
	"github.com/grafana/grafana/pkg/services/ngalert/store"

	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/datasourceproxy"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier"
	"github.com/grafana/grafana/pkg/services/ngalert/schedule"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/tsdb"
)

const (
	maxAttempts int64 = 3
	// scheduler interval
	// changing this value is discouraged
	// because this could cause existing alert definition
	// with intervals that are not exactly divided by this number
	// not to be evaluated
	defaultBaseIntervalSeconds = 10
	// default alert definition interval
	defaultIntervalSeconds int64 = 6 * defaultBaseIntervalSeconds
)

func ProvideService(cfg *setting.Cfg, dataSourceCache datasources.CacheService, routeRegister routing.RouteRegister,
	sqlStore *sqlstore.SQLStore, dataService *tsdb.Service, dataProxy *datasourceproxy.DataSourceProxyService,
	quotaService *quota.QuotaService, m *metrics.Metrics) (*AlertNG, error) {
	ng := &AlertNG{
		Cfg:             cfg,
		DataSourceCache: dataSourceCache,
		RouteRegister:   routeRegister,
		SQLStore:        sqlStore,
		DataService:     dataService,
		DataProxy:       dataProxy,
		QuotaService:    quotaService,
		Metrics:         m,
		Log:             log.New("ngalert"),
	}

	if ng.IsDisabled() {
		return ng, nil
	}

	if err := ng.init(); err != nil {
		return nil, err
	}

	return ng, nil
}

// AlertNG is the service for evaluating the condition of an alert definition.
type AlertNG struct {
	Cfg             *setting.Cfg
	DataSourceCache datasources.CacheService
	RouteRegister   routing.RouteRegister
	SQLStore        *sqlstore.SQLStore
	DataService     *tsdb.Service
	DataProxy       *datasourceproxy.DataSourceProxyService
	QuotaService    *quota.QuotaService
	Metrics         *metrics.Metrics
	Log             log.Logger
	schedule        schedule.ScheduleService
	stateManager    *state.Manager

	// Alerting notification services
	MultiOrgAlertmanager *notifier.MultiOrgAlertmanager
}

func (ng *AlertNG) init() error {
	baseInterval := ng.Cfg.AlertingBaseInterval
	if baseInterval <= 0 {
		baseInterval = defaultBaseIntervalSeconds
	}
	baseInterval *= time.Second

	store := &store.DBstore{
		BaseInterval:           baseInterval,
		DefaultIntervalSeconds: defaultIntervalSeconds,
		SQLStore:               ng.SQLStore,
		Logger:                 ng.Log,
	}

	ng.MultiOrgAlertmanager = notifier.NewMultiOrgAlertmanager(ng.Cfg, store, store)

	// Let's make sure we're able to complete an initial sync of Alertmanagers before we start the alerting components.
	if err := ng.MultiOrgAlertmanager.LoadAndSyncAlertmanagersForOrgs(context.Background()); err != nil {
		return err
	}

	schedCfg := schedule.SchedulerCfg{
		C:                       clock.New(),
		BaseInterval:            baseInterval,
		Logger:                  log.New("ngalert.scheduler"),
		MaxAttempts:             maxAttempts,
		Evaluator:               eval.Evaluator{Cfg: ng.Cfg, Log: ng.Log},
		InstanceStore:           store,
		RuleStore:               store,
		AdminConfigStore:        store,
		OrgStore:                store,
		MultiOrgNotifier:        ng.MultiOrgAlertmanager,
		Metrics:                 ng.Metrics,
		AdminConfigPollInterval: ng.Cfg.AdminConfigPollInterval,
	}
	stateManager := state.NewManager(ng.Log, ng.Metrics, store, store)
	schedule := schedule.NewScheduler(schedCfg, ng.DataService, ng.Cfg.AppURL, stateManager)

	ng.stateManager = stateManager
	ng.schedule = schedule

	api := api.API{
		Cfg:                  ng.Cfg,
		DatasourceCache:      ng.DataSourceCache,
		RouteRegister:        ng.RouteRegister,
		DataService:          ng.DataService,
		Schedule:             ng.schedule,
		DataProxy:            ng.DataProxy,
		QuotaService:         ng.QuotaService,
		InstanceStore:        store,
		RuleStore:            store,
		AlertingStore:        store,
		AdminConfigStore:     store,
		MultiOrgAlertmanager: ng.MultiOrgAlertmanager,
		StateManager:         ng.stateManager,
	}
	api.RegisterAPIEndpoints(ng.Metrics)

	return nil
}

// Run starts the scheduler and Alertmanager.
func (ng *AlertNG) Run(ctx context.Context) error {
	ng.Log.Debug("ngalert starting")
	ng.stateManager.Warm()

	children, subCtx := errgroup.WithContext(ctx)
	children.Go(func() error {
		return ng.schedule.Run(subCtx)
	})
	children.Go(func() error {
		return ng.MultiOrgAlertmanager.Run(subCtx)
	})
	return children.Wait()
}

// IsDisabled returns true if the alerting service is disable for this instance.
func (ng *AlertNG) IsDisabled() bool {
	if ng.Cfg == nil {
		return true
	}
	return !ng.Cfg.IsNgAlertEnabled()
}
