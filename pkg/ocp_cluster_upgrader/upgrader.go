package ocp_cluster_upgrader

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/pkg/apis/upgrade/v1alpha1"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/configmanager"
	"github.com/openshift/managed-upgrade-operator/pkg/drain"
	"github.com/openshift/managed-upgrade-operator/pkg/eventmanager"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/maintenance"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
)

var (
	steps                  UpgradeSteps
	osdUpgradeStepOrdering = []upgradev1alpha1.UpgradeConditionType{
		upgradev1alpha1.ControlPlaneMaintWindow,
		upgradev1alpha1.CommenceUpgrade,
		upgradev1alpha1.ControlPlaneUpgraded,
		upgradev1alpha1.RemoveControlPlaneMaintWindow,
		upgradev1alpha1.WorkersMaintWindow,
		upgradev1alpha1.AllWorkerNodesUpgraded,
		upgradev1alpha1.PostUpgradeVerification,
		upgradev1alpha1.RemoveMaintWindow,
	}
)

// Represents a named series of steps as part of an upgrade process
type UpgradeSteps map[upgradev1alpha1.UpgradeConditionType]UpgradeStep

// Represents an individual step in the upgrade process
type UpgradeStep func(client.Client, *ocpUpgradeConfig, drain.NodeDrainStrategyBuilder, metrics.Metrics, maintenance.Maintenance, cv.ClusterVersion, *upgradev1alpha1.UpgradeConfig, machinery.Machinery, logr.Logger) (bool, error)

// Represents the order in which to undertake upgrade steps
type UpgradeStepOrdering []upgradev1alpha1.UpgradeConditionType

func NewClient(c client.Client, cfm configmanager.ConfigManager, mc metrics.Metrics, notifier eventmanager.EventManager) (*ocpClusterUpgrader, error) {
	cfg := &ocpUpgradeConfig{}
	err := cfm.Into(cfg)
	if err != nil {
		return nil, err
	}

	m, err := maintenance.NewBuilder().NewClient(c)
	if err != nil {
		return nil, err
	}

	steps = map[upgradev1alpha1.UpgradeConditionType]UpgradeStep{
		upgradev1alpha1.ControlPlaneMaintWindow:       CreateControlPlaneMaintWindow,
		upgradev1alpha1.CommenceUpgrade:               CommenceUpgrade,
		upgradev1alpha1.ControlPlaneUpgraded:          ControlPlaneUpgraded,
		upgradev1alpha1.RemoveControlPlaneMaintWindow: RemoveControlPlaneMaintWindow,
		upgradev1alpha1.WorkersMaintWindow:            CreateWorkerMaintWindow,
		upgradev1alpha1.AllWorkerNodesUpgraded:        AllWorkersUpgraded,
		upgradev1alpha1.PostUpgradeVerification:       PostUpgradeVerification,
		upgradev1alpha1.RemoveMaintWindow:             RemoveMaintWindow,
	}

	return &ocpClusterUpgrader{
		Steps:                steps,
		Ordering:             osdUpgradeStepOrdering,
		client:               c,
		maintenance:          m,
		metrics:              mc,
		drainstrategyBuilder: drain.NewBuilder(),
		cvClient:             cv.NewCVClient(c),
		cfg:                  cfg,
		machinery:            machinery.NewMachinery(),
	}, nil
}

// An OSD cluster upgrader implementing the ClusterUpgrader interface
type ocpClusterUpgrader struct {
	Steps                UpgradeSteps
	Ordering             UpgradeStepOrdering
	client               client.Client
	maintenance          maintenance.Maintenance
	metrics              metrics.Metrics
	drainstrategyBuilder drain.NodeDrainStrategyBuilder
	cvClient             cv.ClusterVersion
	cfg                  *ocpUpgradeConfig
	machinery            machinery.Machinery
}

// CommenceUpgrade will update the clusterversion object to apply the desired version to trigger real OCP upgrade
func CommenceUpgrade(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	upgradeCommenced, err := cvClient.HasUpgradeCommenced(upgradeConfig)
	if err != nil {
		return false, err
	}
	desired := upgradeConfig.Spec.Desired
	if upgradeCommenced {
		logger.Info(fmt.Sprintf("ClusterVersion is already set to Channel %s Version %s, skipping %s", desired.Channel, desired.Version, upgradev1alpha1.CommenceUpgrade))
		return true, nil
	}

	logger.Info(fmt.Sprintf("Setting ClusterVersion to Channel %s, version %s", desired.Channel, desired.Version))
	isComplete, err := cvClient.EnsureDesiredVersion(upgradeConfig)
	if err != nil {
		return false, err
	}

	return isComplete, nil
}

// CreateControlPlaneMaintWindow creates the maintenance window for control plane
func CreateControlPlaneMaintWindow(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	endTime := time.Now().Add(cfg.Maintenance.GetControlPlaneDuration())
	err := m.StartControlPlane(endTime, upgradeConfig.Spec.Desired.Version, cfg.Maintenance.IgnoredAlerts.ControlPlaneCriticals)
	if err != nil {
		return false, err
	}

	return true, nil
}

// RemoveControlPlaneMaintWindow removes the maintenance window for control plane
func RemoveControlPlaneMaintWindow(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	err := m.EndControlPlane()
	if err != nil {
		return false, err
	}

	return true, nil
}

// CreateWorkerMaintWindow creates the maintenance window for workers
func CreateWorkerMaintWindow(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	upgradingResult, err := machinery.IsUpgrading(c, "worker")
	if err != nil {
		return false, err
	}

	// Depending on how long the Control Plane takes all workers may be already upgraded.
	if !upgradingResult.IsUpgrading {
		logger.Info(fmt.Sprintf("Worker nodes are already upgraded. Skipping worker maintenace for %s", upgradeConfig.Spec.Desired.Version))
		return true, nil
	}

	pendingWorkerCount := upgradingResult.MachineCount - upgradingResult.UpdatedCount
	if pendingWorkerCount < 1 {
		logger.Info("No worker node left for upgrading.")
		return true, nil
	}

	// We use the maximum of the PDB drain timeout and node drain timeout to compute a 'worst case' wait time
	pdbForceDrainTimeout := time.Duration(upgradeConfig.Spec.PDBForceDrainTimeout) * time.Minute
	nodeDrainTimeout := cfg.NodeDrain.GetTimeOutDuration()
	waitTimePeriod := time.Duration(pendingWorkerCount) * pdbForceDrainTimeout
	if pdbForceDrainTimeout < nodeDrainTimeout {
		waitTimePeriod = time.Duration(pendingWorkerCount) * nodeDrainTimeout
	}

	// Action time is the expected time taken to upgrade a worker node
	maintenanceDurationPerNode := cfg.NodeDrain.GetExpectedDrainDuration()
	actionTimePeriod := time.Duration(pendingWorkerCount) * maintenanceDurationPerNode

	// Our worker maintenance window is a combination of 'wait time' and 'action time'
	totalWorkerMaintenanceDuration := waitTimePeriod + actionTimePeriod

	endTime := time.Now().Add(totalWorkerMaintenanceDuration)
	logger.Info(fmt.Sprintf("Creating worker node maintenace for %d remaining nodes if no previous silence, ending at %v", pendingWorkerCount, endTime))
	err = m.SetWorker(endTime, upgradeConfig.Spec.Desired.Version, pendingWorkerCount)
	if err != nil {
		return false, err
	}

	return true, nil
}

// AllWorkersUpgraded checks whether all the worker nodes are ready with new config
func AllWorkersUpgraded(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	upgradingResult, errUpgrade := machinery.IsUpgrading(c, "worker")
	if errUpgrade != nil {
		return false, errUpgrade
	}

	silenceActive, errSilence := m.IsActive()
	if errSilence != nil {
		return false, errSilence
	}

	if upgradingResult.IsUpgrading {
		logger.Info(fmt.Sprintf("not all workers are upgraded, upgraded: %v, total: %v", upgradingResult.UpdatedCount, upgradingResult.MachineCount))
		if !silenceActive {
			logger.Info("Worker upgrade timeout.")
			metricsClient.UpdateMetricUpgradeWorkerTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version)
		} else {
			metricsClient.ResetMetricUpgradeWorkerTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version)
		}
		return false, nil
	}

	metricsClient.ResetMetricUpgradeWorkerTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version)
	return true, nil
}

// PostUpgradeVerification run the verification steps which defined in performUpgradeVerification
func PostUpgradeVerification(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	ok, err := performUpgradeVerification(c, cfg, metricsClient, logger)
	if err != nil || !ok {
		metricsClient.UpdateMetricClusterVerificationFailed(upgradeConfig.Name)
		return false, err
	}

	metricsClient.UpdateMetricClusterVerificationSucceeded(upgradeConfig.Name)
	return true, nil
}

// performPostUpgradeVerification verifies all replicasets are at expected counts and all daemonsets are at expected counts
func performUpgradeVerification(c client.Client, cfg *ocpUpgradeConfig, metricsClient metrics.Metrics, logger logr.Logger) (bool, error) {

	namespacePrefixesToCheck := cfg.Verification.NamespacePrefixesToCheck
	namespaceToIgnore := cfg.Verification.IgnoredNamespaces

	// Verify all ReplicaSets in the default, kube* and openshfit* namespaces are satisfied
	replicaSetList := &appsv1.ReplicaSetList{}
	err := c.List(context.TODO(), replicaSetList)
	if err != nil {
		return false, err
	}
	readyRs := 0
	totalRs := 0
	for _, replicaSet := range replicaSetList.Items {
		for _, namespacePrefix := range namespacePrefixesToCheck {
			for _, ingoredNS := range namespaceToIgnore {
				if strings.HasPrefix(replicaSet.Namespace, namespacePrefix) && replicaSet.Namespace != ingoredNS {
					totalRs = totalRs + 1
					if replicaSet.Status.ReadyReplicas == replicaSet.Status.Replicas {
						readyRs = readyRs + 1
					}
				}
			}
		}
	}
	if totalRs != readyRs {
		logger.Info(fmt.Sprintf("not all replicaset are ready:expected number :%v , ready number %v", totalRs, readyRs))
		return false, nil
	}

	// Verify all Daemonsets in the default, kube* and openshift* namespaces are satisfied
	daemonSetList := &appsv1.DaemonSetList{}
	err = c.List(context.TODO(), daemonSetList)
	if err != nil {
		return false, err
	}
	readyDS := 0
	totalDS := 0
	for _, daemonSet := range daemonSetList.Items {
		for _, namespacePrefix := range namespacePrefixesToCheck {
			for _, ignoredNS := range namespaceToIgnore {
				if strings.HasPrefix(daemonSet.Namespace, namespacePrefix) && daemonSet.Namespace != ignoredNS {
					totalDS = totalDS + 1
					if daemonSet.Status.DesiredNumberScheduled == daemonSet.Status.NumberReady {
						readyDS = readyDS + 1
					}
				}
			}
		}
	}
	if totalDS != readyDS {
		logger.Info(fmt.Sprintf("not all daemonset are ready:expected number :%v , ready number %v", totalDS, readyDS))
		return false, nil
	}

	// If daemonsets and replicasets are satisfied, any active TargetDown alerts will eventually go away.
	// Wait for that to occur before declaring the verification complete.
	namespacePrefixesAsRegex := make([]string, 0)
	namespaceIgnoreAlert := make([]string, 0)
	for _, namespacePrefix := range namespacePrefixesToCheck {
		namespacePrefixesAsRegex = append(namespacePrefixesAsRegex, fmt.Sprintf("^%s-.*", namespacePrefix))
	}
	namespaceIgnoreAlert = append(namespaceIgnoreAlert, namespaceToIgnore...)
	isTargetDownFiring, err := metricsClient.IsAlertFiring("TargetDown", namespacePrefixesAsRegex, namespaceIgnoreAlert)
	if err != nil {
		return false, fmt.Errorf("can't query for alerts: %v", err)
	}
	if isTargetDownFiring {
		logger.Info(fmt.Sprintf("TargetDown alerts are still firing in namespaces %v", namespacePrefixesAsRegex))
		return false, nil
	}

	return true, nil
}

// RemoveMaintWindows removes all the maintenance windows we created during the upgrade
func RemoveMaintWindow(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	err := m.EndWorker()
	if err != nil {
		return false, err
	}

	return true, nil
}

// ControlPlaneUpgraded checks whether control plane is upgraded. The ClusterVersion reports when cvo and master nodes are upgraded.
func ControlPlaneUpgraded(c client.Client, cfg *ocpUpgradeConfig, dsb drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, logger logr.Logger) (bool, error) {
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return false, err
	}

	isCompleted := cvClient.HasUpgradeCompleted(clusterVersion, upgradeConfig)
	history := cv.GetHistory(clusterVersion, upgradeConfig.Spec.Desired.Version)
	if history == nil {
		return false, err
	}

	upgradeStartTime := history.StartedTime
	controlPlaneCompleteTime := history.CompletionTime
	upgradeTimeout := cfg.Maintenance.GetControlPlaneDuration()
	if !upgradeStartTime.IsZero() && controlPlaneCompleteTime == nil && time.Now().After(upgradeStartTime.Add(upgradeTimeout)) {
		logger.Info("Control plane upgrade timeout")
		metricsClient.UpdateMetricUpgradeControlPlaneTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version)
	}

	if isCompleted {
		metricsClient.ResetMetricUpgradeControlPlaneTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version)
		return true, nil
	}

	return false, nil
}

// This trigger the upgrade process
func (cu ocpClusterUpgrader) UpgradeCluster(upgradeConfig *upgradev1alpha1.UpgradeConfig, logger logr.Logger) (upgradev1alpha1.UpgradePhase, *upgradev1alpha1.UpgradeCondition, error) {
	logger.Info("Upgrading cluster")

	for _, key := range cu.Ordering {

		logger.Info(fmt.Sprintf("Performing %s", key))

		result, err := cu.Steps[key](cu.client, cu.cfg, cu.drainstrategyBuilder, cu.metrics, cu.maintenance, cu.cvClient, upgradeConfig, cu.machinery, logger)

		if err != nil {
			logger.Error(err, fmt.Sprintf("Error when %s", key))
			condition := newUpgradeCondition(fmt.Sprintf("%s not done", key), err.Error(), key, corev1.ConditionFalse)
			return upgradev1alpha1.UpgradePhaseUpgrading, condition, err
		}
		if !result {
			logger.Info(fmt.Sprintf("%s not done, skip following steps", key))
			condition := newUpgradeCondition(fmt.Sprintf("%s not done", key), fmt.Sprintf("%s still in progress", key), key, corev1.ConditionFalse)
			return upgradev1alpha1.UpgradePhaseUpgrading, condition, nil
		}
	}

	key := cu.Ordering[len(cu.Ordering)-1]
	condition := newUpgradeCondition(fmt.Sprintf("%s done", key), fmt.Sprintf("%s is completed", key), key, corev1.ConditionTrue)
	return upgradev1alpha1.UpgradePhaseUpgraded, condition, nil
}

// check several things about the cluster and report problems
// * critical alerts
// * degraded operators (if there are critical alerts only)
func performClusterHealthCheck(c client.Client, metricsClient metrics.Metrics, cvClient cv.ClusterVersion, cfg *ocpUpgradeConfig, logger logr.Logger) (bool, error) {
	ic := cfg.HealthCheck.IgnoredCriticals
	icQuery := ""
	if len(ic) > 0 {
		icQuery = `,alertname!="` + strings.Join(ic, `",alertname!="`) + `"`
	}
	healthCheckQuery := `ALERTS{alertstate="firing",severity="critical",namespace=~"^openshift.*|^kube.*|^default$",namespace!="openshift-customer-monitoring",namespace!="openshift-logging"` + icQuery + "}"
	alerts, err := metricsClient.Query(healthCheckQuery)
	if err != nil {
		return false, fmt.Errorf("Unable to query critical alerts: %s", err)
	}

	if len(alerts.Data.Result) > 0 {
		logger.Info("There are critical alerts exists, cannot upgrade now")
		return false, fmt.Errorf("There are %d critical alerts", len(alerts.Data.Result))
	}

	result, err := cvClient.HasDegradedOperators()
	if err != nil {
		return false, err
	}
	if len(result.Degraded) > 0 {
		logger.Info(fmt.Sprintf("degraded operators :%s", strings.Join(result.Degraded, ",")))
		// Send the metrics for the cluster check failed if we have degraded operators
		return false, fmt.Errorf("degraded operators :%s", strings.Join(result.Degraded, ","))
	}

	return true, nil
}

func newUpgradeCondition(reason, msg string, conditionType upgradev1alpha1.UpgradeConditionType, s corev1.ConditionStatus) *upgradev1alpha1.UpgradeCondition {
	return &upgradev1alpha1.UpgradeCondition{
		Type:    conditionType,
		Status:  s,
		Reason:  reason,
		Message: msg,
	}
}

