package protocol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	kafkav1beta2 "github.com/RedHatInsights/strimzi-client-go/apis/kafka.strimzi.io/v1beta2"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/go-logr/logr"
	subv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stolostron/multicluster-global-hub/operator/apis/v1alpha4"
	operatorv1alpha4 "github.com/stolostron/multicluster-global-hub/operator/apis/v1alpha4"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/config"
	operatorconstants "github.com/stolostron/multicluster-global-hub/operator/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/controllers/addon/certificates"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/deployer"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/renderer"
	"github.com/stolostron/multicluster-global-hub/operator/pkg/utils"
	operatorutils "github.com/stolostron/multicluster-global-hub/operator/pkg/utils"
	"github.com/stolostron/multicluster-global-hub/pkg/constants"
	"github.com/stolostron/multicluster-global-hub/pkg/transport"
)

const (
	KafkaClusterName = "kafka"
	// kafka storage
	DefaultKafkaDefaultStorageSize = "10Gi"

	// Global hub kafkaUser name
	DefaultGlobalHubKafkaUserName = "global-hub-kafka-user"

	// subscription - common
	DefaultKafkaSubName           = "strimzi-kafka-operator"
	DefaultInstallPlanApproval    = subv1alpha1.ApprovalAutomatic
	DefaultCatalogSourceNamespace = "openshift-marketplace"

	// subscription - production
	DefaultAMQChannel        = "amq-streams-2.7.x"
	DefaultAMQPackageName    = "amq-streams"
	DefaultCatalogSourceName = "redhat-operators"

	// subscription - community
	CommunityChannel           = "strimzi-0.40.x"
	CommunityPackageName       = "strimzi-kafka-operator"
	CommunityCatalogSourceName = "community-operators"
)

var (
	KafkaStorageIdentifier   int32 = 0
	KafkaStorageDeleteClaim        = false
	KafkaVersion                   = "3.7.0"
	DefaultPartition         int32 = 1
	DefaultPartitionReplicas int32 = 3
	// kafka metrics constants
	KakfaMetricsConfigmapName       = "kafka-metrics"
	KafkaMetricsConfigmapKeyRef     = "kafka-metrics-config.yml"
	ZooKeeperMetricsConfigmapKeyRef = "zookeeper-metrics-config.yml"
)

// install the strimzi kafka cluster by operator
type strimziTransporter struct {
	log                   logr.Logger
	ctx                   context.Context
	kafkaClusterName      string
	kafkaClusterNamespace string

	// subscription properties
	subName              string
	subCommunity         bool
	subChannel           string
	subCatalogSourceName string
	subPackageName       string

	// global hub config
	mgh           *operatorv1alpha4.MulticlusterGlobalHub
	runtimeClient client.Client
	manager       ctrl.Manager

	// wait until kafka cluster status is ready when initialize
	waitReady bool
	enableTLS bool
	// default is false, to create topic for each managed hub
	sharedTopics           bool
	topicPartitionReplicas int32
}

type KafkaOption func(*strimziTransporter)

func NewStrimziTransporter(mgr ctrl.Manager, mgh *operatorv1alpha4.MulticlusterGlobalHub,
	opts ...KafkaOption,
) (*strimziTransporter, error) {
	k := &strimziTransporter{
		log:                   ctrl.Log.WithName("strimzi-transporter"),
		ctx:                   context.TODO(),
		kafkaClusterName:      KafkaClusterName,
		kafkaClusterNamespace: mgh.Namespace,

		subName:              DefaultKafkaSubName,
		subCommunity:         false,
		subChannel:           DefaultAMQChannel,
		subPackageName:       DefaultAMQPackageName,
		subCatalogSourceName: DefaultCatalogSourceName,

		waitReady:              true,
		enableTLS:              true,
		sharedTopics:           false,
		topicPartitionReplicas: DefaultPartitionReplicas,

		manager:       mgr,
		runtimeClient: mgr.GetClient(),
		mgh:           mgh,
	}
	// apply options
	for _, opt := range opts {
		opt(k)
	}

	if k.subCommunity {
		k.subChannel = CommunityChannel
		k.subPackageName = CommunityPackageName
		k.subCatalogSourceName = CommunityCatalogSourceName
	}

	if mgh.Spec.AvailabilityConfig == operatorv1alpha4.HABasic {
		k.topicPartitionReplicas = 1
	}

	err := k.ensureKafka(k.mgh)
	if err != nil {
		return nil, err
	}

	// use the client ca to sign the csr for the managed hubs
	if err := config.SetClientCA(k.ctx, mgh.Namespace, KafkaClusterName, k.runtimeClient); err != nil {
		return nil, err
	}
	return k, err
}

func WithNamespacedName(name types.NamespacedName) KafkaOption {
	return func(sk *strimziTransporter) {
		sk.kafkaClusterName = name.Name
		sk.kafkaClusterNamespace = name.Namespace
	}
}

func WithContext(ctx context.Context) KafkaOption {
	return func(sk *strimziTransporter) {
		sk.ctx = ctx
	}
}

func WithCommunity(val bool) KafkaOption {
	return func(sk *strimziTransporter) {
		sk.subCommunity = val
	}
}

func WithSubName(name string) KafkaOption {
	return func(sk *strimziTransporter) {
		sk.subName = name
	}
}

func WithWaitReady(wait bool) KafkaOption {
	return func(sk *strimziTransporter) {
		sk.waitReady = wait
	}
}

// ensureKafka the kafka subscription, cluster, metrics, global hub user and topic
func (k *strimziTransporter) ensureKafka(mgh *operatorv1alpha4.MulticlusterGlobalHub) error {
	k.log.Info("reconcile global hub kafka transport...")
	err := k.ensureSubscription(mgh)
	if err != nil {
		return err
	}
	err = wait.PollUntilContextTimeout(k.ctx, 2*time.Second, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if !config.GetKafkaResourceReady() {
				return false, fmt.Errorf("the kafka crds is not ready")
			}
			err, _ = k.CreateUpdateKafkaCluster(mgh)
			if err != nil {
				k.log.Info("the kafka cluster is not created, retrying...", "message", err.Error())
				return false, nil
			}
			// kafka metrics, monitor, global hub kafkaTopic and kafkaUser
			err = k.renderKafkaResources(mgh)
			if err != nil {
				k.log.Info("the kafka resources are not created, retrying...", "message", err.Error())
				return false, nil
			}

			return true, nil
		})
	if err != nil {
		return err
	}

	if !k.waitReady {
		return nil
	}

	if err := k.kafkaClusterReady(); err != nil {
		return err
	}

	return nil
}

// renderKafkaMetricsResources renders the kafka podmonitor and metrics, and kafkaUser and kafkaTopic for global hub
func (k *strimziTransporter) renderKafkaResources(mgh *v1alpha4.MulticlusterGlobalHub) error {
	statusTopic := config.GetRawStatusTopic()
	statusPlaceholderTopic := config.GetRawStatusTopic()
	topicParttern := kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypeLiteral
	if strings.Contains(config.GetRawStatusTopic(), "*") {
		statusTopic = strings.Replace(config.GetRawStatusTopic(), "*", "", -1)
		statusPlaceholderTopic = strings.Replace(config.GetRawStatusTopic(), "*", "global-hub", -1)
		topicParttern = kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypePrefix
	}
	// render the kafka objects
	kafkaRenderer, kafkaDeployer := renderer.NewHoHRenderer(manifests), deployer.NewHoHDeployer(k.manager.GetClient())
	kafkaObjects, err := kafkaRenderer.Render("manifests", "",
		func(profile string) (interface{}, error) {
			return struct {
				EnableMetrics          bool
				Namespace              string
				KafkaCluster           string
				GlobalHubKafkaUser     string
				SpecTopic              string
				StatusTopic            string
				StatusTopicParttern    string
				StatusPlaceholderTopic string
				TopicPartition         int32
				TopicReplicas          int32
			}{
				EnableMetrics:          mgh.Spec.EnableMetrics,
				Namespace:              mgh.GetNamespace(),
				KafkaCluster:           KafkaClusterName,
				GlobalHubKafkaUser:     DefaultGlobalHubKafkaUserName,
				SpecTopic:              config.GetSpecTopic(),
				StatusTopic:            statusTopic,
				StatusTopicParttern:    string(topicParttern),
				StatusPlaceholderTopic: statusPlaceholderTopic,
				TopicPartition:         DefaultPartition,
				TopicReplicas:          DefaultPartitionReplicas,
			}, nil
		})
	if err != nil {
		return fmt.Errorf("failed to render kafka manifests: %w", err)
	}
	// create restmapper for deployer to find GVR
	dc, err := discovery.NewDiscoveryClientForConfig(k.manager.GetConfig())
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	if err = operatorutils.ManipulateGlobalHubObjects(kafkaObjects, mgh, kafkaDeployer, mapper,
		k.manager.GetScheme()); err != nil {
		return fmt.Errorf("failed to create/update kafka objects: %w", err)
	}
	return nil
}

// EnsureUser to reconcile the kafkaUser's setting(authn and authz)
func (k *strimziTransporter) EnsureUser(clusterName string) (string, error) {
	userName := config.GetKafkaUserName(clusterName)
	clusterTopic := k.getClusterTopic(clusterName)

	authnType := kafkav1beta2.KafkaUserSpecAuthenticationTypeTlsExternal
	simpleACLs := []kafkav1beta2.KafkaUserSpecAuthorizationAclsElem{
		ConsumeGroupReadACL(),
		ReadTopicACL(clusterTopic.SpecTopic, false),
		WriteTopicACL(clusterTopic.StatusTopic),
	}

	desiredKafkaUser := k.newKafkaUser(userName, authnType, simpleACLs)

	kafkaUser := &kafkav1beta2.KafkaUser{}
	err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
		Name:      userName,
		Namespace: k.kafkaClusterNamespace,
	}, kafkaUser)
	if errors.IsNotFound(err) {
		klog.Infof("create the kafakUser: %s", userName)
		return userName, k.runtimeClient.Create(k.ctx, desiredKafkaUser, &client.CreateOptions{})
	} else if err != nil {
		return "", err
	}

	updatedKafkaUser := &kafkav1beta2.KafkaUser{}
	err = utils.MergeObjects(kafkaUser, desiredKafkaUser, updatedKafkaUser)
	if err != nil {
		return "", err
	}

	if !equality.Semantic.DeepDerivative(updatedKafkaUser.Spec, kafkaUser.Spec) {
		klog.Infof("update the kafkaUser: %s", userName)
		if err = k.runtimeClient.Update(k.ctx, updatedKafkaUser); err != nil {
			return "", err
		}
	}
	return userName, nil
}

func (k *strimziTransporter) EnsureTopic(clusterName string) (*transport.ClusterTopic, error) {
	clusterTopic := k.getClusterTopic(clusterName)

	topicNames := []string{clusterTopic.SpecTopic, clusterTopic.StatusTopic}

	for _, topicName := range topicNames {
		kafkaTopic := &kafkav1beta2.KafkaTopic{}
		err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
			Name:      topicName,
			Namespace: k.kafkaClusterNamespace,
		}, kafkaTopic)
		if errors.IsNotFound(err) {
			if e := k.runtimeClient.Create(k.ctx, k.newKafkaTopic(topicName)); e != nil {
				return nil, e
			}
			continue // reconcile the next topic
		} else if err != nil {
			return nil, err
		}

		// update the topic
		desiredTopic := k.newKafkaTopic(topicName)

		updatedTopic := &kafkav1beta2.KafkaTopic{}
		err = utils.MergeObjects(kafkaTopic, desiredTopic, updatedTopic)
		if err != nil {
			return nil, err
		}
		// Kafka do not support change exitsting kafaka topic replica directly.
		updatedTopic.Spec.Replicas = kafkaTopic.Spec.Replicas

		if !equality.Semantic.DeepDerivative(updatedTopic.Spec, kafkaTopic.Spec) {
			if err = k.runtimeClient.Update(k.ctx, updatedTopic); err != nil {
				return nil, err
			}
		}
	}
	return clusterTopic, nil
}

func (k *strimziTransporter) Prune(clusterName string) error {
	// cleanup kafkaUser
	kafkaUser := &kafkav1beta2.KafkaUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.GetKafkaUserName(clusterName),
			Namespace: k.kafkaClusterNamespace,
		},
	}
	err := k.runtimeClient.Get(k.ctx, client.ObjectKeyFromObject(kafkaUser), kafkaUser)
	if err == nil {
		if err := k.runtimeClient.Delete(k.ctx, kafkaUser); err != nil {
			return err
		}
	} else if !errors.IsNotFound(err) {
		return err
	}

	// only delete the topic when removing the CR, otherwise the manager throws error like "Unknown topic or partition"
	// if k.sharedTopics || !strings.Contains(config.GetRawStatusTopic(), "*") {
	// 	return nil
	// }

	// clusterTopic := k.getClusterTopic(clusterName)
	// kafkaTopic := &kafkav1beta2.KafkaTopic{
	// 	ObjectMeta: metav1.ObjectMeta{
	// 		Name:      clusterTopic.StatusTopic,
	// 		Namespace: k.kafkaClusterNamespace,
	// 	},
	// }
	// err = k.runtimeClient.Get(k.ctx, client.ObjectKeyFromObject(kafkaTopic), kafkaTopic)
	// if err == nil {
	// 	if err := k.runtimeClient.Delete(k.ctx, kafkaTopic); err != nil {
	// 		return err
	// 	}
	// } else if !errors.IsNotFound(err) {
	// 	return err
	// }

	return nil
}

func (k *strimziTransporter) getClusterTopic(clusterName string) *transport.ClusterTopic {
	topic := &transport.ClusterTopic{
		SpecTopic:   config.GetSpecTopic(),
		StatusTopic: config.GetStatusTopic(clusterName),
	}
	return topic
}

// the username is the kafkauser, it's the same as the secret name
func (k *strimziTransporter) GetConnCredential(clusterName string) (*transport.KafkaConnCredential, error) {
	// bootstrapServer, clusterId, clusterCA
	credential, err := k.getConnCredentailByCluster()
	if err != nil {
		return nil, err
	}

	// certificates
	credential.CASecretName = GetClusterCASecret(k.kafkaClusterName)
	credential.ClientSecretName = certificates.AgentCertificateSecretName()

	// topics
	credential.StatusTopic = config.GetStatusTopic(clusterName)
	credential.SpecTopic = config.GetSpecTopic()

	// don't need to load the client cert/key from the kafka user, since it use the external kafkaUser
	// userName := config.GetKafkaUserName(clusterName)
	// if !k.enableTLS {
	// 	k.log.Info("the kafka cluster hasn't enable tls for user", "username", userName)
	// 	return credential, nil
	// }
	// if err := k.loadUserCredentail(userName, credential); err != nil {
	// 	return nil, err
	// }
	return credential, nil
}

func GetClusterCASecret(clusterName string) string {
	return fmt.Sprintf("%s-cluster-ca-cert", clusterName)
}

// loadUserCredentail add credential with client cert, and key
func (k *strimziTransporter) loadUserCredentail(kafkaUserName string, credential *transport.KafkaConnCredential) error {
	kafkaUserSecret := &corev1.Secret{}
	err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
		Name:      kafkaUserName,
		Namespace: k.kafkaClusterNamespace,
	}, kafkaUserSecret)
	if err != nil {
		return err
	}
	credential.ClientCert = base64.StdEncoding.EncodeToString(kafkaUserSecret.Data["user.crt"])
	credential.ClientKey = base64.StdEncoding.EncodeToString(kafkaUserSecret.Data["user.key"])
	return nil
}

// getConnCredentailByCluster gets credential with clusterId, bootstrapServer, and serverCA
func (k *strimziTransporter) getConnCredentailByCluster() (*transport.KafkaConnCredential, error) {
	kafkaCluster := &kafkav1beta2.Kafka{}
	err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
		Name:      k.kafkaClusterName,
		Namespace: k.kafkaClusterNamespace,
	}, kafkaCluster)
	if err != nil {
		return nil, err
	}

	if kafkaCluster.Status == nil || kafkaCluster.Status.Conditions == nil {
		return nil, fmt.Errorf("kafka cluster %s has no status conditions", kafkaCluster.Name)
	}

	for _, condition := range kafkaCluster.Status.Conditions {
		if *condition.Type == "Ready" && *condition.Status == "True" {
			clusterIdentity := string(kafkaCluster.GetUID())
			if kafkaCluster.Status.ClusterId != nil {
				clusterIdentity = *kafkaCluster.Status.ClusterId
			}
			credential := &transport.KafkaConnCredential{
				ClusterID:       clusterIdentity,
				BootstrapServer: *kafkaCluster.Status.Listeners[1].BootstrapServers,
				CACert:          base64.StdEncoding.EncodeToString([]byte(kafkaCluster.Status.Listeners[1].Certificates[0])),
			}
			return credential, nil
		}
	}
	return nil, fmt.Errorf("kafka cluster %s/%s is not ready", k.kafkaClusterNamespace, k.kafkaClusterName)
}

func (k *strimziTransporter) newKafkaTopic(topicName string) *kafkav1beta2.KafkaTopic {
	return &kafkav1beta2.KafkaTopic{
		ObjectMeta: metav1.ObjectMeta{
			Name:      topicName,
			Namespace: k.kafkaClusterNamespace,
			Labels: map[string]string{
				// It is important to set the cluster label otherwise the topic will not be ready
				"strimzi.io/cluster":             k.kafkaClusterName,
				constants.GlobalHubOwnerLabelKey: constants.GlobalHubOwnerLabelVal,
			},
		},
		Spec: &kafkav1beta2.KafkaTopicSpec{
			Partitions: &DefaultPartition,
			Replicas:   &k.topicPartitionReplicas,
			Config: &apiextensions.JSON{Raw: []byte(`{
				"cleanup.policy": "compact"
			}`)},
		},
	}
}

func (k *strimziTransporter) newKafkaUser(
	userName string,
	authnType kafkav1beta2.KafkaUserSpecAuthenticationType,
	simpleACLs []kafkav1beta2.KafkaUserSpecAuthorizationAclsElem,
) *kafkav1beta2.KafkaUser {
	return &kafkav1beta2.KafkaUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      userName,
			Namespace: k.kafkaClusterNamespace,
			Labels: map[string]string{
				// It is important to set the cluster label otherwise the user will not be ready
				"strimzi.io/cluster":             k.kafkaClusterName,
				constants.GlobalHubOwnerLabelKey: constants.GlobalHubOwnerLabelVal,
			},
		},
		Spec: &kafkav1beta2.KafkaUserSpec{
			Authentication: &kafkav1beta2.KafkaUserSpecAuthentication{
				Type: authnType,
			},
			Authorization: &kafkav1beta2.KafkaUserSpecAuthorization{
				Type: kafkav1beta2.KafkaUserSpecAuthorizationTypeSimple,
				Acls: simpleACLs,
			},
		},
	}
}

// waits for kafka cluster to be ready and returns nil if kafka cluster ready
func (k *strimziTransporter) kafkaClusterReady() error {
	k.log.Info("waiting the kafka cluster instance to be ready...")

	err := wait.PollUntilContextTimeout(k.ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			// if the mgh is deleted/deleting, then don't need to wait the kafka is ready
			e := k.runtimeClient.Get(ctx, config.GetMGHNamespacedName(), k.mgh)
			if e != nil {
				return false, e
			}
			if k.mgh.DeletionTimestamp != nil {
				return false, fmt.Errorf("deleting the mgh: %s, no need waiting the kafka ready", k.mgh.Namespace)
			}

			kafkaCluster := &kafkav1beta2.Kafka{}
			err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
				Name:      k.kafkaClusterName,
				Namespace: k.kafkaClusterNamespace,
			}, kafkaCluster)
			if err != nil {
				k.log.V(2).Info("fail to get the kafka cluster, waiting", "message", err.Error())
				return false, nil
			}
			if kafkaCluster.Status == nil || kafkaCluster.Status.Conditions == nil {
				k.log.V(2).Info("kafka cluster status is not ready")
				return false, nil
			}

			if kafkaCluster.Spec != nil && kafkaCluster.Spec.Kafka.Listeners != nil {
				// if the kafka cluster is already created, check if the tls is enabled
				enableTLS := false
				for _, listener := range kafkaCluster.Spec.Kafka.Listeners {
					if listener.Tls {
						enableTLS = true
						break
					}
				}
				k.enableTLS = enableTLS
			}

			for _, condition := range kafkaCluster.Status.Conditions {
				if *condition.Type == "Ready" && *condition.Status == "True" {
					k.log.Info("kafka cluster is ready")
					return true, nil
				}
			}

			k.log.V(2).Info("kafka cluster status condition is not ready")
			return false, nil
		})
	return err
}

func (k *strimziTransporter) CreateUpdateKafkaCluster(mgh *operatorv1alpha4.MulticlusterGlobalHub) (error, bool) {
	existingKafka := &kafkav1beta2.Kafka{}
	err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
		Name:      k.kafkaClusterName,
		Namespace: mgh.Namespace,
	}, existingKafka)
	if err != nil {
		if errors.IsNotFound(err) {
			return k.runtimeClient.Create(k.ctx, k.newKafkaCluster(mgh)), true
		}
		return err, false
	}

	// this is only for e2e test. patch the kafka needs more time to be ready
	if _, ok := existingKafka.Annotations["skip-patch-if-exist"]; ok {
		return nil, false
	}

	desiredKafka := k.newKafkaCluster(mgh)

	updatedKafka := &kafkav1beta2.Kafka{}
	err = utils.MergeObjects(existingKafka, desiredKafka, updatedKafka)
	if err != nil {
		return err, false
	}

	updatedKafka.Spec.Kafka.MetricsConfig = desiredKafka.Spec.Kafka.MetricsConfig
	updatedKafka.Spec.Zookeeper.MetricsConfig = desiredKafka.Spec.Zookeeper.MetricsConfig

	if !reflect.DeepEqual(updatedKafka.Spec, existingKafka.Spec) {
		return k.runtimeClient.Update(k.ctx, updatedKafka), true
	}
	return nil, false
}

func (k *strimziTransporter) getKafkaResources(
	mgh *operatorv1alpha4.MulticlusterGlobalHub,
) *kafkav1beta2.KafkaSpecKafkaResources {
	kafkaRes := utils.GetResources(operatorconstants.Kafka, mgh.Spec.AdvancedConfig)
	kafkaSpecRes := &kafkav1beta2.KafkaSpecKafkaResources{}
	jsonData, err := json.Marshal(kafkaRes)
	if err != nil {
		k.log.Error(err, "failed to marshal kafka resources")
	}
	err = json.Unmarshal(jsonData, kafkaSpecRes)
	if err != nil {
		k.log.Error(err, "failed to unmarshal to KafkaSpecKafkaResources")
	}

	return kafkaSpecRes
}

func (k *strimziTransporter) getZookeeperResources(
	mgh *operatorv1alpha4.MulticlusterGlobalHub,
) *kafkav1beta2.KafkaSpecZookeeperResources {
	zookeeperRes := utils.GetResources(operatorconstants.Zookeeper, mgh.Spec.AdvancedConfig)

	zookeeperSpecRes := &kafkav1beta2.KafkaSpecZookeeperResources{}
	jsonData, err := json.Marshal(zookeeperRes)
	if err != nil {
		k.log.Error(err, "failed to marshal zookeeper resources")
	}
	err = json.Unmarshal(jsonData, zookeeperSpecRes)
	if err != nil {
		k.log.Error(err, "failed to unmarshal to KafkaSpecZookeeperResources")
	}
	return zookeeperSpecRes
}

func (k *strimziTransporter) newKafkaCluster(mgh *operatorv1alpha4.MulticlusterGlobalHub) *kafkav1beta2.Kafka {
	storageSize := config.GetKafkaStorageSize(mgh)
	kafkaSpecKafkaStorageVolumesElem := kafkav1beta2.KafkaSpecKafkaStorageVolumesElem{
		Id:          &KafkaStorageIdentifier,
		Size:        &storageSize,
		Type:        kafkav1beta2.KafkaSpecKafkaStorageVolumesElemTypePersistentClaim,
		DeleteClaim: &KafkaStorageDeleteClaim,
	}
	kafkaSpecZookeeperStorage := kafkav1beta2.KafkaSpecZookeeperStorage{
		Type:        kafkav1beta2.KafkaSpecZookeeperStorageTypePersistentClaim,
		Size:        &storageSize,
		DeleteClaim: &KafkaStorageDeleteClaim,
	}

	if mgh.Spec.DataLayer.StorageClass != "" {
		kafkaSpecKafkaStorageVolumesElem.Class = &mgh.Spec.DataLayer.StorageClass
		kafkaSpecZookeeperStorage.Class = &mgh.Spec.DataLayer.StorageClass
	}

	kafkaCluster := &kafkav1beta2.Kafka{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k.kafkaClusterName,
			Namespace: k.kafkaClusterNamespace,
			Labels: map[string]string{
				constants.GlobalHubOwnerLabelKey: constants.GlobalHubOwnerLabelVal,
			},
		},
		Spec: &kafkav1beta2.KafkaSpec{
			Kafka: kafkav1beta2.KafkaSpecKafka{
				Config: &apiextensions.JSON{Raw: []byte(`{
"default.replication.factor": 3,
"inter.broker.protocol.version": "3.7",
"min.insync.replicas": 2,
"offsets.topic.replication.factor": 3,
"transaction.state.log.min.isr": 2,
"transaction.state.log.replication.factor": 3
}`)},
				Listeners: []kafkav1beta2.KafkaSpecKafkaListenersElem{
					{
						Name: "plain",
						Port: 9092,
						Tls:  false,
						Type: kafkav1beta2.KafkaSpecKafkaListenersElemTypeInternal,
					},
					{
						Name: "tls",
						Port: 9093,
						Tls:  true,
						Type: kafkav1beta2.KafkaSpecKafkaListenersElemTypeRoute,
						Authentication: &kafkav1beta2.KafkaSpecKafkaListenersElemAuthentication{
							Type: kafkav1beta2.KafkaSpecKafkaListenersElemAuthenticationTypeTls,
						},
					},
				},
				Resources: k.getKafkaResources(mgh),
				Authorization: &kafkav1beta2.KafkaSpecKafkaAuthorization{
					Type: kafkav1beta2.KafkaSpecKafkaAuthorizationTypeSimple,
				},
				Replicas: 3,
				Storage: kafkav1beta2.KafkaSpecKafkaStorage{
					Type: kafkav1beta2.KafkaSpecKafkaStorageTypeJbod,
					Volumes: []kafkav1beta2.KafkaSpecKafkaStorageVolumesElem{
						kafkaSpecKafkaStorageVolumesElem,
					},
				},
				Version: &KafkaVersion,
			},
			Zookeeper: kafkav1beta2.KafkaSpecZookeeper{
				Replicas:  3,
				Storage:   kafkaSpecZookeeperStorage,
				Resources: k.getZookeeperResources(mgh),
			},
			EntityOperator: &kafkav1beta2.KafkaSpecEntityOperator{
				TopicOperator: &kafkav1beta2.KafkaSpecEntityOperatorTopicOperator{},
				UserOperator:  &kafkav1beta2.KafkaSpecEntityOperatorUserOperator{},
			},
		},
	}

	k.setAffinity(mgh, kafkaCluster)
	k.setTolerations(mgh, kafkaCluster)
	k.setMetricsConfig(mgh, kafkaCluster)
	k.setImagePullSecret(mgh, kafkaCluster)

	return kafkaCluster
}

// set metricsConfig for kafka cluster based on the mgh enableMetrics
func (k *strimziTransporter) setMetricsConfig(mgh *operatorv1alpha4.MulticlusterGlobalHub,
	kafkaCluster *kafkav1beta2.Kafka,
) {
	kafkaMetricsConfig := &kafkav1beta2.KafkaSpecKafkaMetricsConfig{}
	zookeeperMetricsConfig := &kafkav1beta2.KafkaSpecZookeeperMetricsConfig{}
	if mgh.Spec.EnableMetrics {
		kafkaMetricsConfig = &kafkav1beta2.KafkaSpecKafkaMetricsConfig{
			Type: kafkav1beta2.KafkaSpecKafkaMetricsConfigTypeJmxPrometheusExporter,
			ValueFrom: kafkav1beta2.KafkaSpecKafkaMetricsConfigValueFrom{
				ConfigMapKeyRef: &kafkav1beta2.KafkaSpecKafkaMetricsConfigValueFromConfigMapKeyRef{
					Name: &KakfaMetricsConfigmapName,
					Key:  &KafkaMetricsConfigmapKeyRef,
				},
			},
		}
		zookeeperMetricsConfig = &kafkav1beta2.KafkaSpecZookeeperMetricsConfig{
			Type: kafkav1beta2.KafkaSpecZookeeperMetricsConfigTypeJmxPrometheusExporter,
			ValueFrom: kafkav1beta2.KafkaSpecZookeeperMetricsConfigValueFrom{
				ConfigMapKeyRef: &kafkav1beta2.KafkaSpecZookeeperMetricsConfigValueFromConfigMapKeyRef{
					Name: &KakfaMetricsConfigmapName,
					Key:  &ZooKeeperMetricsConfigmapKeyRef,
				},
			},
		}
		kafkaCluster.Spec.Kafka.MetricsConfig = kafkaMetricsConfig
		kafkaCluster.Spec.Zookeeper.MetricsConfig = zookeeperMetricsConfig
	}
}

// set affinity for kafka cluster based on the mgh nodeSelector
func (k *strimziTransporter) setAffinity(mgh *operatorv1alpha4.MulticlusterGlobalHub,
	kafkaCluster *kafkav1beta2.Kafka,
) {
	kafkaPodAffinity := &kafkav1beta2.KafkaSpecKafkaTemplatePodAffinity{}
	zookeeperPodAffinity := &kafkav1beta2.KafkaSpecZookeeperTemplatePodAffinity{}
	entityOperatorPodAffinity := &kafkav1beta2.KafkaSpecEntityOperatorTemplatePodAffinity{}

	if mgh.Spec.NodeSelector != nil {
		nodeSelectorReqs := []corev1.NodeSelectorRequirement{}

		for key, value := range mgh.Spec.NodeSelector {
			req := corev1.NodeSelectorRequirement{
				Key:      key,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{value},
			}
			nodeSelectorReqs = append(nodeSelectorReqs, req)
		}
		nodeSelectorTerms := []corev1.NodeSelectorTerm{
			{
				MatchExpressions: nodeSelectorReqs,
			},
		}

		jsonData, err := json.Marshal(nodeSelectorTerms)
		if err != nil {
			k.log.Error(err, "failed to marshall nodeSelector terms")
		}

		kafkaNodeSelectorTermsElem := make([]kafkav1beta2.
			KafkaSpecKafkaTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecutionNodeSelectorTermsElem,
			0)

		zookeeperNodeSelectorTermsElem := make([]kafkav1beta2.
			KafkaSpecZookeeperTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecutionNodeSelectorTermsElem, 0)
		entityOperatorNodeSelectorTermsElem := make([]kafkav1beta2.
			KafkaSpecEntityOperatorTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecutionNodeSelectorTermsElem, 0)

		err = json.Unmarshal(jsonData, &kafkaNodeSelectorTermsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to kafkaNodeSelectorTermsElem")
		}
		err = json.Unmarshal(jsonData, &zookeeperNodeSelectorTermsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to zookeeperNodeSelectorTermsElem")
		}
		err = json.Unmarshal(jsonData, &entityOperatorNodeSelectorTermsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to entityOperatorNodeSelectorTermsElem")
		}

		zookeeperPodAffinity.NodeAffinity = &kafkav1beta2.KafkaSpecZookeeperTemplatePodAffinityNodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &kafkav1beta2.
				KafkaSpecZookeeperTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecution{
				NodeSelectorTerms: zookeeperNodeSelectorTermsElem,
			},
		}
		kafkaPodAffinity.NodeAffinity = &kafkav1beta2.KafkaSpecKafkaTemplatePodAffinityNodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &kafkav1beta2.
				KafkaSpecKafkaTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecution{
				NodeSelectorTerms: kafkaNodeSelectorTermsElem,
			},
		}
		entityOperatorPodAffinity.NodeAffinity = &kafkav1beta2.KafkaSpecEntityOperatorTemplatePodAffinityNodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &kafkav1beta2.
				KafkaSpecEntityOperatorTemplatePodAffinityNodeAffinityRequiredDuringSchedulingIgnoredDuringExecution{
				NodeSelectorTerms: entityOperatorNodeSelectorTermsElem,
			},
		}

		if kafkaCluster.Spec.Kafka.Template == nil {
			kafkaCluster.Spec.Kafka.Template = &kafkav1beta2.KafkaSpecKafkaTemplate{
				Pod: &kafkav1beta2.KafkaSpecKafkaTemplatePod{
					Affinity: kafkaPodAffinity,
				},
			}
			kafkaCluster.Spec.Zookeeper.Template = &kafkav1beta2.KafkaSpecZookeeperTemplate{
				Pod: &kafkav1beta2.KafkaSpecZookeeperTemplatePod{
					Affinity: zookeeperPodAffinity,
				},
			}
			kafkaCluster.Spec.EntityOperator.Template = &kafkav1beta2.KafkaSpecEntityOperatorTemplate{
				Pod: &kafkav1beta2.KafkaSpecEntityOperatorTemplatePod{
					Affinity: entityOperatorPodAffinity,
				},
			}
		} else {
			kafkaCluster.Spec.Kafka.Template.Pod.Affinity = kafkaPodAffinity
			kafkaCluster.Spec.Zookeeper.Template.Pod.Affinity = zookeeperPodAffinity
			kafkaCluster.Spec.EntityOperator.Template.Pod.Affinity = entityOperatorPodAffinity
		}
	}
}

// setTolerations sets the kafka tolerations based on the mgh tolerations
func (k *strimziTransporter) setTolerations(mgh *operatorv1alpha4.MulticlusterGlobalHub,
	kafkaCluster *kafkav1beta2.Kafka,
) {
	kafkaTolerationsElem := make([]kafkav1beta2.KafkaSpecKafkaTemplatePodTolerationsElem, 0)
	zookeeperTolerationsElem := make([]kafkav1beta2.KafkaSpecZookeeperTemplatePodTolerationsElem, 0)
	entityOperatorTolerationsElem := make([]kafkav1beta2.KafkaSpecEntityOperatorTemplatePodTolerationsElem, 0)

	if mgh.Spec.Tolerations != nil {
		jsonData, err := json.Marshal(mgh.Spec.Tolerations)
		if err != nil {
			k.log.Error(err, "failed to marshal tolerations")
		}
		err = json.Unmarshal(jsonData, &kafkaTolerationsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to KafkaSpecruntimeKafkaTemplatePodTolerationsElem")
		}
		err = json.Unmarshal(jsonData, &zookeeperTolerationsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to KafkaSpecZookeeperTemplatePodTolerationsElem")
		}
		err = json.Unmarshal(jsonData, &entityOperatorTolerationsElem)
		if err != nil {
			k.log.Error(err, "failed to unmarshal to KafkaSpecEntityOperatorTemplatePodTolerationsElem")
		}

		if kafkaCluster.Spec.Kafka.Template == nil {
			kafkaCluster.Spec.Kafka.Template = &kafkav1beta2.KafkaSpecKafkaTemplate{
				Pod: &kafkav1beta2.KafkaSpecKafkaTemplatePod{
					Tolerations: kafkaTolerationsElem,
				},
			}
			kafkaCluster.Spec.Zookeeper.Template = &kafkav1beta2.KafkaSpecZookeeperTemplate{
				Pod: &kafkav1beta2.KafkaSpecZookeeperTemplatePod{
					Tolerations: zookeeperTolerationsElem,
				},
			}
			kafkaCluster.Spec.EntityOperator.Template = &kafkav1beta2.KafkaSpecEntityOperatorTemplate{
				Pod: &kafkav1beta2.KafkaSpecEntityOperatorTemplatePod{
					Tolerations: entityOperatorTolerationsElem,
				},
			}
		} else {
			kafkaCluster.Spec.Kafka.Template.Pod.Tolerations = kafkaTolerationsElem
			kafkaCluster.Spec.Zookeeper.Template.Pod.Tolerations = zookeeperTolerationsElem
			kafkaCluster.Spec.EntityOperator.Template.Pod.Tolerations = entityOperatorTolerationsElem
		}
	}
}

// setImagePullSecret sets the kafka image pull secret based on the mgh imagepullsecret
func (k *strimziTransporter) setImagePullSecret(mgh *operatorv1alpha4.MulticlusterGlobalHub,
	kafkaCluster *kafkav1beta2.Kafka,
) {
	if mgh.Spec.ImagePullSecret != "" {
		existingKafkaSpec := kafkaCluster.Spec
		desiredKafkaSpec := kafkaCluster.Spec.DeepCopy()
		desiredKafkaSpec.EntityOperator.Template = &kafkav1beta2.KafkaSpecEntityOperatorTemplate{
			Pod: &kafkav1beta2.KafkaSpecEntityOperatorTemplatePod{
				ImagePullSecrets: []kafkav1beta2.KafkaSpecEntityOperatorTemplatePodImagePullSecretsElem{
					{
						Name: &mgh.Spec.ImagePullSecret,
					},
				},
			},
		}
		desiredKafkaSpec.Kafka.Template = &kafkav1beta2.KafkaSpecKafkaTemplate{
			Pod: &kafkav1beta2.KafkaSpecKafkaTemplatePod{
				ImagePullSecrets: []kafkav1beta2.KafkaSpecKafkaTemplatePodImagePullSecretsElem{
					{
						Name: &mgh.Spec.ImagePullSecret,
					},
				},
			},
		}
		desiredKafkaSpec.Zookeeper.Template = &kafkav1beta2.KafkaSpecZookeeperTemplate{
			Pod: &kafkav1beta2.KafkaSpecZookeeperTemplatePod{
				ImagePullSecrets: []kafkav1beta2.KafkaSpecZookeeperTemplatePodImagePullSecretsElem{
					{
						Name: &mgh.Spec.ImagePullSecret,
					},
				},
			},
		}
		// marshal to json
		existingKafkaJson, _ := json.Marshal(existingKafkaSpec)
		desiredKafkaJson, _ := json.Marshal(desiredKafkaSpec)

		// patch the desired kafka cluster to the existing kafka cluster
		patchedData, err := jsonpatch.MergePatch(existingKafkaJson, desiredKafkaJson)
		if err != nil {
			klog.Errorf("failed to merge patch, error: %v", err)
			return
		}

		updatedKafkaSpec := &kafkav1beta2.KafkaSpec{}
		err = json.Unmarshal(patchedData, updatedKafkaSpec)
		if err != nil {
			klog.Errorf("failed to umarshal kafkaspec, error: %v", err)
			return
		}
		kafkaCluster.Spec = updatedKafkaSpec
	}
}

// create/ update the kafka subscription
func (k *strimziTransporter) ensureSubscription(mgh *operatorv1alpha4.MulticlusterGlobalHub) error {
	// get subscription
	existingSub := &subv1alpha1.Subscription{}
	err := k.runtimeClient.Get(k.ctx, types.NamespacedName{
		Name:      k.subName,
		Namespace: mgh.GetNamespace(),
	}, existingSub)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	expectedSub := k.newSubscription(mgh)
	if errors.IsNotFound(err) {
		return k.runtimeClient.Create(k.ctx, expectedSub)
	} else {
		startingCSV := expectedSub.Spec.StartingCSV
		// if updating channel must remove startingCSV
		if existingSub.Spec.Channel != expectedSub.Spec.Channel {
			startingCSV = ""
		}
		if !equality.Semantic.DeepEqual(existingSub.Spec, expectedSub.Spec) {
			existingSub.Spec = expectedSub.Spec
		}
		existingSub.Spec.StartingCSV = startingCSV
		return k.runtimeClient.Update(k.ctx, existingSub)
	}
}

// newSubscription returns an CrunchyPostgres subscription with desired default values
func (k *strimziTransporter) newSubscription(mgh *operatorv1alpha4.MulticlusterGlobalHub) *subv1alpha1.Subscription {
	labels := map[string]string{
		"installer.name":                 mgh.Name,
		"installer.namespace":            mgh.Namespace,
		constants.GlobalHubOwnerLabelKey: constants.GHOperatorOwnerLabelVal,
	}
	// Generate sub config from mgh CR
	subConfig := &subv1alpha1.SubscriptionConfig{
		NodeSelector: mgh.Spec.NodeSelector,
		Tolerations:  mgh.Spec.Tolerations,
	}

	sub := &subv1alpha1.Subscription{
		TypeMeta: metav1.TypeMeta{
			APIVersion: subv1alpha1.SubscriptionCRDAPIVersion,
			Kind:       subv1alpha1.SubscriptionKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      k.subName,
			Namespace: mgh.Namespace,
			Labels:    labels,
		},
		Spec: &subv1alpha1.SubscriptionSpec{
			Channel:                k.subChannel,
			InstallPlanApproval:    DefaultInstallPlanApproval,
			Package:                k.subPackageName,
			CatalogSource:          k.subCatalogSourceName,
			CatalogSourceNamespace: DefaultCatalogSourceNamespace,
			Config:                 subConfig,
		},
	}
	return sub
}

func WriteTopicACL(topicName string) kafkav1beta2.KafkaUserSpecAuthorizationAclsElem {
	host := "*"
	patternType := kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypeLiteral
	writeAcl := kafkav1beta2.KafkaUserSpecAuthorizationAclsElem{
		Host: &host,
		Resource: kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResource{
			Type:        kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourceTypeTopic,
			Name:        &topicName,
			PatternType: &patternType,
		},
		Operations: []kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElem{
			kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElemWrite,
		},
	}
	return writeAcl
}

func ReadTopicACL(topicName string, prefixParttern bool) kafkav1beta2.KafkaUserSpecAuthorizationAclsElem {
	host := "*"
	patternType := kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypeLiteral
	if prefixParttern {
		patternType = kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypePrefix
	}

	return kafkav1beta2.KafkaUserSpecAuthorizationAclsElem{
		Host: &host,
		Resource: kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResource{
			Type:        kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourceTypeTopic,
			Name:        &topicName,
			PatternType: &patternType,
		},
		Operations: []kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElem{
			kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElemDescribe,
			kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElemRead,
		},
	}
}

func ConsumeGroupReadACL() kafkav1beta2.KafkaUserSpecAuthorizationAclsElem {
	host := "*"
	consumerGroup := "*"
	consumerPatternType := kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourcePatternTypeLiteral
	consumerAcl := kafkav1beta2.KafkaUserSpecAuthorizationAclsElem{
		Host: &host,
		Resource: kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResource{
			Type:        kafkav1beta2.KafkaUserSpecAuthorizationAclsElemResourceTypeGroup,
			Name:        &consumerGroup,
			PatternType: &consumerPatternType,
		},
		Operations: []kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElem{
			kafkav1beta2.KafkaUserSpecAuthorizationAclsElemOperationsElemRead,
		},
	}
	return consumerAcl
}
