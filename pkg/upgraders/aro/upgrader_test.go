package aro

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/pkg/apis/upgrade/v1alpha1"
	ac "github.com/openshift/managed-upgrade-operator/pkg/availabilitychecks"
	acMocks "github.com/openshift/managed-upgrade-operator/pkg/availabilitychecks/mocks"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	cvMocks "github.com/openshift/managed-upgrade-operator/pkg/clusterversion/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/drain"
	mockDrain "github.com/openshift/managed-upgrade-operator/pkg/drain/mocks"
	em "github.com/openshift/managed-upgrade-operator/pkg/eventmanager"
	emMocks "github.com/openshift/managed-upgrade-operator/pkg/eventmanager/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	mockMachinery "github.com/openshift/managed-upgrade-operator/pkg/machinery/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/maintenance"
	mockMaintenance "github.com/openshift/managed-upgrade-operator/pkg/maintenance/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	mockMetrics "github.com/openshift/managed-upgrade-operator/pkg/metrics/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/notifier"
	"github.com/openshift/managed-upgrade-operator/pkg/scaler"
	mockScaler "github.com/openshift/managed-upgrade-operator/pkg/scaler/mocks"
	"github.com/openshift/managed-upgrade-operator/util/mocks"
	testStructs "github.com/openshift/managed-upgrade-operator/util/mocks/structs"
)

var stepCounter map[upgradev1alpha1.UpgradeConditionType]int
var _ = Describe("ClusterUpgrader", func() {
	var (
		logger logr.Logger
		// mocks
		mockKubeClient           *mocks.MockClient
		mockCtrl                 *gomock.Controller
		mockMaintClient          *mockMaintenance.MockMaintenance
		mockScalerClient         *mockScaler.MockScaler
		mockMachineryClient      *mockMachinery.MockMachinery
		mockMetricsClient        *mockMetrics.MockMetrics
		mockCVClient             *cvMocks.MockClusterVersion
		mockDrainStrategyBuilder *mockDrain.MockNodeDrainStrategyBuilder
		mockEMClient             *emMocks.MockEventManager
		mockAC                   *acMocks.MockAvailabilityChecker
		// upgradeconfig to be used during tests
		upgradeConfigName types.NamespacedName
		upgradeConfig     *upgradev1alpha1.UpgradeConfig
		//	upgradeCommencedCV *configv1.ClusterVersion
		config *aroUpgradeConfig
	)

	BeforeEach(func() {
		upgradeConfigName = types.NamespacedName{
			Name:      "test-upgradeconfig",
			Namespace: "test-namespace",
		}
		upgradeConfig = testStructs.NewUpgradeConfigBuilder().WithNamespacedName(upgradeConfigName).GetUpgradeConfig()
		mockCtrl = gomock.NewController(GinkgoT())
		mockKubeClient = mocks.NewMockClient(mockCtrl)
		mockMaintClient = mockMaintenance.NewMockMaintenance(mockCtrl)
		mockMetricsClient = mockMetrics.NewMockMetrics(mockCtrl)
		mockScalerClient = mockScaler.NewMockScaler(mockCtrl)
		mockMachineryClient = mockMachinery.NewMockMachinery(mockCtrl)
		mockCVClient = cvMocks.NewMockClusterVersion(mockCtrl)
		mockDrainStrategyBuilder = mockDrain.NewMockNodeDrainStrategyBuilder(mockCtrl)
		mockEMClient = emMocks.NewMockEventManager(mockCtrl)
		mockAC = acMocks.NewMockAvailabilityChecker(mockCtrl)
		logger = logf.Log.WithName("cluster upgrader test logger")
		stepCounter = make(map[upgradev1alpha1.UpgradeConditionType]int)
		config = &aroUpgradeConfig{
			Maintenance: maintenanceConfig{
				ControlPlaneTime: 90,
			},
			Scale: scaleConfig{
				TimeOut: 30,
			},
			NodeDrain: drain.NodeDrain{
				ExpectedNodeDrainTime: 8,
			},
			UpgradeWindow: upgradeWindow{
				TimeOut:      120,
				DelayTrigger: 30,
			},
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("When assessing if the control plane is upgraded to a version", func() {
		Context("When the clusterversion can't be fetched", func() {
			It("Indicates an error", func() {
				fakeError := fmt.Errorf("fake error")
				mockCVClient.EXPECT().GetClusterVersion().Return(nil, fakeError)
				result, err := ControlPlaneUpgraded(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(HaveOccurred())
				Expect(err).To(Equal(fakeError))
				Expect(result).To(BeFalse())
			})
		})

		Context("When that version is recorded in clusterversion's history", func() {
			var clusterVersion *configv1.ClusterVersion
			BeforeEach(func() {
				clusterVersion = &configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						History: []configv1.UpdateHistory{
							{State: configv1.CompletedUpdate, Version: "something"},
							{State: configv1.CompletedUpdate, Version: upgradeConfig.Spec.Desired.Version, StartedTime: metav1.Time{Time: time.Now()}},
							{State: configv1.CompletedUpdate, Version: "something else"},
						},
					},
				}
			})
			It("Flags the control plane as upgraded", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(clusterVersion, nil),
					mockCVClient.EXPECT().HasUpgradeCompleted(gomock.Any(), gomock.Any()).Return(true),
					mockMetricsClient.EXPECT().ResetMetricUpgradeControlPlaneTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version),
				)
				result, err := ControlPlaneUpgraded(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})

		Context("When the control plane hasn't upgraded within the window", func() {
			var clusterVersion *configv1.ClusterVersion
			upgradeStartTime := time.Now().Add(-300 * time.Minute)
			BeforeEach(func() {
				clusterVersion = &configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						History: []configv1.UpdateHistory{
							{State: configv1.PartialUpdate, Version: upgradeConfig.Spec.Desired.Version, StartedTime: metav1.Time{Time: upgradeStartTime}},
						},
					},
				}
			})
			It("Sets the appropriate metric", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().GetClusterVersion().Return(clusterVersion, nil),
					mockCVClient.EXPECT().HasUpgradeCompleted(gomock.Any(), gomock.Any()).Return(false),
					mockMetricsClient.EXPECT().UpdateMetricUpgradeControlPlaneTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version),
				)
				result, err := ControlPlaneUpgraded(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
			})
		})
	})

	Context("Scaling", func() {
		Context("When capacity reservation is enabled", func() {
			It("Should scale up extra nodes and set success metric on successful scaling when capacity reservation enabled", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockScalerClient.EXPECT().EnsureScaleUpNodes(gomock.Any(), config.GetScaleDuration(), gomock.Any()).Return(true, nil),
					mockMetricsClient.EXPECT().UpdateMetricScalingSucceeded(gomock.Any()),
				)

				ok, err := EnsureExtraUpgradeWorkers(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(ok).To(BeTrue())
			})
			It("Should set failed metric on scaling time out when capacity reservation enabled", func() {
				gomock.InOrder(
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockScalerClient.EXPECT().EnsureScaleUpNodes(gomock.Any(), config.GetScaleDuration(), gomock.Any()).Return(false, scaler.NewScaleTimeOutError("test scale timed out")),
					mockMetricsClient.EXPECT().UpdateMetricScalingFailed(gomock.Any()),
				)

				ok, err := EnsureExtraUpgradeWorkers(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(HaveOccurred())
				Expect(ok).To(BeFalse())
			})
		})
		Context("When capacity reservation is disabled", func() {
			BeforeEach(func() {
				upgradeConfig.Spec.CapacityReservation = false
			})
			It("Should not scale up extra nodes", func() {
				ok, err := EnsureExtraUpgradeWorkers(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(ok).To(BeTrue())
			})
			It("Shoud not scale down extra nodes", func() {
				ok, err := RemoveExtraScaledNodes(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(Not(HaveOccurred()))
				Expect(ok).To(BeTrue())
			})
		})
	})

	Context("When requesting the cluster to begin upgrading", func() {
		Context("When the clusterversion version can't be fetched", func() {
			It("Indicates an error", func() {
				fakeError := fmt.Errorf("a fake error")
				gomock.InOrder(
					mockMetricsClient.EXPECT().UpdateMetricUpgradeWindowNotBreached(gomock.Any()),
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, fakeError),
				)
				result, err := CommenceUpgrade(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(HaveOccurred())
				Expect(err).To(Equal(fakeError))
				Expect(result).To(BeFalse())
			})
		})

		Context("When setting the desired version fails", func() {
			It("Indicates an error", func() {
				fakeError := fmt.Errorf("fake error")
				gomock.InOrder(
					mockMetricsClient.EXPECT().UpdateMetricUpgradeWindowNotBreached(gomock.Any()),
					mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
					mockCVClient.EXPECT().EnsureDesiredVersion(gomock.Any()).Return(false, fakeError),
				)
				result, err := CommenceUpgrade(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).To(HaveOccurred())
				Expect(err).To(Equal(fakeError))
				Expect(result).To(BeFalse())
			})
		})
	})

	Context("When assessing whether all workers are upgraded", func() {
		Context("When all workers are upgraded", func() {
			It("Indicates that all workers are upgraded", func() {
				gomock.InOrder(
					mockMachineryClient.EXPECT().IsUpgrading(gomock.Any(), "worker").Return(&machinery.UpgradingResult{IsUpgrading: false}, nil),
					mockMaintClient.EXPECT().IsActive(),
					mockMetricsClient.EXPECT().ResetMetricUpgradeWorkerTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version),
				)
				result, err := AllWorkersUpgraded(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
			})
		})
		Context("When all workers are not upgraded", func() {
			It("Indicates that all workers are not upgraded", func() {
				gomock.InOrder(
					mockMachineryClient.EXPECT().IsUpgrading(gomock.Any(), "worker").Return(&machinery.UpgradingResult{IsUpgrading: true}, nil),
					mockMaintClient.EXPECT().IsActive(),
					mockMetricsClient.EXPECT().UpdateMetricUpgradeWorkerTimeout(upgradeConfig.Name, upgradeConfig.Spec.Desired.Version),
				)
				result, err := AllWorkersUpgraded(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
			})
		})
	})

	Context("When the cluster's upgrade process has commenced", func() {
		It("will not re-perform a pre-upgrade health check", func() {
			gomock.InOrder(
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil),
			)
			result, err := PreClusterHealthCheck(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeTrue())
		})
		It("will not re-perform spinning up extra workers", func() {
			gomock.InOrder(mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil))
			result, err := EnsureExtraUpgradeWorkers(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeTrue())
		})
		It("will not re-perform commencing an upgrade", func() {
			gomock.InOrder(
				mockMetricsClient.EXPECT().UpdateMetricUpgradeWindowNotBreached(gomock.Any()),
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil),
			)
			result, err := CommenceUpgrade(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeTrue())
		})
	})

	Context("When the upgrader can't tell if the cluster's upgrade has commenced", func() {
		var fakeError = fmt.Errorf("fake upgradeCommenced error")
		It("will abort the pre-upgrade health check", func() {
			gomock.InOrder(
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, fakeError),
			)
			result, err := PreClusterHealthCheck(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).To(HaveOccurred())
			Expect(err).To(Equal(fakeError))
			Expect(result).To(BeFalse())
		})
		It("will abort the spinning up of extra workers", func() {
			gomock.InOrder(mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, fakeError))
			result, err := EnsureExtraUpgradeWorkers(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).To(HaveOccurred())
			Expect(err).To(Equal(fakeError))
			Expect(result).To(BeFalse())
		})
		It("will abort the commencing of an upgrade", func() {
			gomock.InOrder(
				mockMetricsClient.EXPECT().UpdateMetricUpgradeWindowNotBreached(gomock.Any()),
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, fakeError),
			)
			result, err := CommenceUpgrade(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).To(HaveOccurred())
			Expect(err).To(Equal(fakeError))
			Expect(result).To(BeFalse())
		})
	})

	Context("When running the external-dependency-availability-check phase", func() {
		It("return true if all dependencies are available", func() {
			gomock.InOrder(
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
				mockAC.EXPECT().AvailabilityCheck().Return(nil),
			)

			result, err := ExternalDependencyAvailabilityCheck(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeTrue())
		})
		It("return false if any of the dependencies are not available", func() {
			fakeErr := fmt.Errorf("fake error")
			gomock.InOrder(
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
				mockAC.EXPECT().AvailabilityCheck().Return(fakeErr),
			)

			result, err := ExternalDependencyAvailabilityCheck(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).To(HaveOccurred())
			Expect(result).To(BeFalse())
		})
		It("will not perform availability checking if the cluster is upgrading", func() {
			gomock.InOrder(
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil),
			)
			result, err := ExternalDependencyAvailabilityCheck(mockKubeClient, config, mockScalerClient, mockDrainStrategyBuilder, mockMetricsClient, mockMaintClient, mockCVClient, mockEMClient, upgradeConfig, mockMachineryClient, []ac.AvailabilityChecker{mockAC}, logger)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeTrue())
		})
	})

	Context("When performing Cluster Upgrade steps", func() {
		var testSteps UpgradeSteps
		var testOrder UpgradeStepOrdering
		var cu *aroClusterUpgrader
		var step1 = upgradev1alpha1.UpgradeValidated
		BeforeEach(func() {
			testOrder = []upgradev1alpha1.UpgradeConditionType{
				step1,
			}
			testSteps = map[upgradev1alpha1.UpgradeConditionType]UpgradeStep{
				step1: makeMockSucceedStep(step1),
			}
			cu = &aroClusterUpgrader{
				Steps:       testSteps,
				Ordering:    testOrder,
				client:      mockKubeClient,
				maintenance: mockMaintClient,
				metrics:     mockMetricsClient,
				cvClient:    mockCVClient,
				notifier:    mockEMClient,
				cfg:         config,
				scaler:      mockScalerClient,
			}
			upgradeConfig.Status.History = []upgradev1alpha1.UpgradeHistory{
				{
					Version: upgradeConfig.Spec.Desired.Version,
					Phase:   upgradev1alpha1.UpgradePhaseUpgrading,
				},
			}
		})

		Context("When a step does not occur in the history", func() {
			BeforeEach(func() {
				cu.Steps = map[upgradev1alpha1.UpgradeConditionType]UpgradeStep{
					step1: makeMockUnsucceededStep(step1),
				}
			})

			It("returns an uncompleted condition for the step", func() {
				// Add a step that will not complete on execution, so we can observe it starting
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil)
				phase, condition, err := cu.UpgradeCluster(upgradeConfig, logger)
				stepHistoryReason := condition.Reason
				Expect(phase).To(Equal(upgradev1alpha1.UpgradePhaseUpgrading))
				Expect(condition.Status).To(Equal(corev1.ConditionFalse))
				Expect(stepHistoryReason).To(Equal(string(step1) + " not done"))
				Expect(err).NotTo(HaveOccurred())
			})

			It("runs the step", func() {
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil)
				_, _, err := cu.UpgradeCluster(upgradeConfig, logger)
				Expect(stepCounter[step1]).To(Equal(1))
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("When running a step returns an error", func() {
			BeforeEach(func() {
				cu.Steps = map[upgradev1alpha1.UpgradeConditionType]UpgradeStep{
					step1: makeMockFailedStep(step1),
				}
			})
			It("Indicates the error in the condition", func() {
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil)
				_, condition, err := cu.UpgradeCluster(upgradeConfig, logger)
				stepHistoryReason := condition.Reason
				stepHistoryMsg := condition.Message
				Expect(stepHistoryReason).To(Equal(string(step1) + " not done"))
				Expect(stepHistoryMsg).To(Equal("step " + string(step1) + " failed"))
				Expect(stepCounter[step1]).To(Equal(1))
				Expect(err).To(HaveOccurred())
			})

		})

		Context("When all steps have indicated completion", func() {
			BeforeEach(func() {
				upgradeConfig.Status.History = []upgradev1alpha1.UpgradeHistory{
					{
						Version: upgradeConfig.Spec.Desired.Version,
						Phase:   upgradev1alpha1.UpgradePhaseUpgrading,
						Conditions: []upgradev1alpha1.UpgradeCondition{
							{
								Type:    step1,
								Status:  corev1.ConditionTrue,
								Reason:  string(step1) + " succeed",
								Message: string(step1) + " succeed",
							},
						},
					},
				}
			})
			It("flags the upgrade as completed", func() {
				mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(true, nil)
				phase, condition, err := cu.UpgradeCluster(upgradeConfig, logger)
				Expect(phase).To(Equal(upgradev1alpha1.UpgradePhaseUpgraded))
				Expect(condition.Status).To(Equal(corev1.ConditionTrue))
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("When the cluster is in a possible failed state", func() {
			Context("When the upgrade hasn't started in its window", func() {
				BeforeEach(func() {
					upgradeStartTime := time.Now().Add(time.Duration(-2*config.UpgradeWindow.TimeOut) * time.Minute)
					upgradeConfig.Status.History = []upgradev1alpha1.UpgradeHistory{
						{
							Version:   upgradeConfig.Spec.Desired.Version,
							Phase:     upgradev1alpha1.UpgradePhaseUpgrading,
							StartTime: &metav1.Time{Time: upgradeStartTime},
							Conditions: []upgradev1alpha1.UpgradeCondition{
								{
									Type:    step1,
									Status:  corev1.ConditionTrue,
									Reason:  string(step1) + " succeed",
									Message: string(step1) + " succeed",
								},
							},
						},
					}
				})
				It("flags the upgrade as failed", func() {
					gomock.InOrder(
						mockCVClient.EXPECT().HasUpgradeCommenced(gomock.Any()).Return(false, nil),
						mockScalerClient.EXPECT().EnsureScaleDownNodes(gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil),
						mockEMClient.EXPECT().Notify(notifier.StateFailed),
						mockMetricsClient.EXPECT().UpdateMetricUpgradeWindowBreached(upgradeConfig.Name),
						mockMetricsClient.EXPECT().ResetFailureMetrics(),
					)
					phase, condition, err := cu.UpgradeCluster(upgradeConfig, logger)
					Expect(phase).To(Equal(upgradev1alpha1.UpgradePhaseFailed))
					Expect(condition.Status).To(Equal(corev1.ConditionTrue))
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

	})

	Context("Unit tests", func() {

		Context("When creating an UpgradeCondition", func() {
			It("Populates all fields properly", func() {
				reason := "testreason"
				msg := "testmsg"
				ucon := upgradev1alpha1.UpgradeConditionType("testuc")
				status := corev1.ConditionTrue
				uc := newUpgradeCondition(reason, msg, ucon, status)
				Expect(uc.Status).To(Equal(status))
				Expect(uc.Message).To(Equal(msg))
				Expect(uc.Reason).To(Equal(reason))
				Expect(uc.Type).To(Equal(ucon))
			})
		})
	})

})

func makeMockSucceedStep(step upgradev1alpha1.UpgradeConditionType) UpgradeStep {
	return func(c client.Client, config *aroUpgradeConfig, scaler scaler.Scaler, drainBuilder drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, emClient em.EventManager, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, availabilityCheckers ac.AvailabilityCheckers, logger logr.Logger) (bool, error) {
		stepCounter[step] += 1
		return true, nil
	}
}

func makeMockUnsucceededStep(step upgradev1alpha1.UpgradeConditionType) UpgradeStep {
	return func(c client.Client, config *aroUpgradeConfig, scaler scaler.Scaler, drainBuilder drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, emClient em.EventManager, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, availabilityCheckers ac.AvailabilityCheckers, logger logr.Logger) (bool, error) {
		stepCounter[step] += 1
		return false, nil
	}
}

func makeMockFailedStep(step upgradev1alpha1.UpgradeConditionType) UpgradeStep {
	return func(c client.Client, config *aroUpgradeConfig, scaler scaler.Scaler, drainBuilder drain.NodeDrainStrategyBuilder, metricsClient metrics.Metrics, m maintenance.Maintenance, cvClient cv.ClusterVersion, emClient em.EventManager, upgradeConfig *upgradev1alpha1.UpgradeConfig, machinery machinery.Machinery, availabilityCheckers ac.AvailabilityCheckers, logger logr.Logger) (bool, error) {
		stepCounter[step] += 1
		return false, fmt.Errorf("step %s failed", step)
	}
}
