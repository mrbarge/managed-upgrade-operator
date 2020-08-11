package osd_cluster_upgrader

import (
	"fmt"
	"time"
)

type osdUpgradeConfig struct {
	Maintenance maintenanceConfig `yaml:"maintenance"`
	Scale       scaleConfig       `yaml:"scale"`
	NodeDrain   nodeDrain         `yaml:"nodeDrain"`
	HealthCheck healthCheck       `yaml:"healthCheck"`
}

type maintenanceConfig struct {
	ControlPlaneTime int           `yaml:"controlPlaneTime"`
	WorkerNodeTime   int           `yaml:"workerNodeTime"`
	IgnoredAlerts    ignoredAlerts `yaml:"ignoredAlerts"`
}

type ignoredAlerts struct {
	// Generally upgrades should not fire critical alerts but there are some critical alerts that will fire.
	// e.g. 'etcdMembersDown' happens as the masters drain/reboot and a master is offline but this is expected and will resolve.
	// This is a list of critical alerts that can be ignored while upgrading of controlplane occurs
	ControlPlaneCriticals []string `yaml:"controlPlaneCriticals"`
}

func (cfg *maintenanceConfig) IsValid() error {
	if cfg.ControlPlaneTime <= 0 {
		return fmt.Errorf("Config maintenace controlPlaneTime out is invalid")
	}
	if cfg.WorkerNodeTime <= 0 {
		return fmt.Errorf("Config maintenace workerNodeTime is invalid")
	}

	return nil
}

func (cfg *maintenanceConfig) GetControlPlaneDuration() time.Duration {
	return time.Duration(cfg.ControlPlaneTime) * time.Minute
}

func (cfg *maintenanceConfig) GetWorkerNodeDuration() time.Duration {
	return time.Duration(cfg.WorkerNodeTime) * time.Minute
}

type scaleConfig struct {
	TimeOut int `yaml:"timeOut"`
}

type nodeDrain struct {
	TimeOut int `yaml:"timeOut"`
}

type healthCheck struct {
	IgnoredCriticals []string `yaml:"ignoredCriticals"`
}

func (cfg *osdUpgradeConfig) IsValid() error {
	if err := cfg.Maintenance.IsValid(); err != nil {
		return err
	}
	if cfg.Scale.TimeOut <= 0 {
		return fmt.Errorf("Config scale timeOut is invalid")
	}
	if cfg.NodeDrain.TimeOut <= 0 {
		return fmt.Errorf("Config nodeDrain timeOut is invalid")
	}

	return nil
}

func (cfg *osdUpgradeConfig) GetScaleDuration() time.Duration {
	return time.Duration(cfg.Scale.TimeOut) * time.Minute
}

func (cfg *osdUpgradeConfig) GetNodeDrainDuration() time.Duration {
	return time.Duration(cfg.NodeDrain.TimeOut) * time.Minute
}
