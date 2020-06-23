package maintenance

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

//go:generate mockgen -destination=mocks/maintenance.go -package=mocks github.com/openshift/managed-upgrade-operator/pkg/maintenance Maintenance
type Maintenance interface {
	StartControlPlane(endsAt time.Time) error
	StartWorker(endsAt time.Time) error
	End() error
}

//go:generate mockgen -destination=mocks/maintenanceBuilder.go -package=$GOPACKAGE github.com/openshift/managed-upgrade-operator/pkg/maintenance MaintenanceBuilder
type MaintenanceBuilder interface {
	NewClient(client client.Client) (Maintenance, error)
}

func NewBuilder() MaintenanceBuilder {
	return &alertManagerMaintenanceBuilder{}
}
