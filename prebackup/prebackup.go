package prebackup

import (
	"fmt"
	"os"
	"path/filepath"

	k8upv1alpha1 "github.com/vshn/k8up/api/v1alpha1"
	"github.com/vshn/k8up/constants"
	"github.com/vshn/k8up/job"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// PreBackup defines a preBackup.
type PreBackup struct {
	job.Config
}

// NewPrebackup returns a new PreBackup. Although it is not a direct job that is being
// triggered, it takes the same config type as the other job types.
func NewPrebackup(config job.Config) *PreBackup {
	return &PreBackup{
		Config: config,
	}
}

// Start will start the defined pods as deployments.
func (p *PreBackup) Start() error {

	templates, err := p.getPodTemplates()
	if err != nil {
		return err
	}

	deployments := p.generateDeployments(templates)

	return p.startAndWaitForReady(deployments)
}

func (p *PreBackup) getPodTemplates() (*k8upv1alpha1.PreBackupPodList, error) {
	podList := &k8upv1alpha1.PreBackupPodList{}

	err := p.Client.List(p.CTX, podList)
	if err != nil {
		return nil, fmt.Errorf("could not list pod templates: %w", err)
	}

	return podList, nil
}

func (p *PreBackup) generateDeployments(templates *k8upv1alpha1.PreBackupPodList) []appsv1.Deployment {
	deployments := make([]appsv1.Deployment, 0)

	if len(templates.Items) == 0 {
		return deployments
	}

	for _, template := range templates.Items {

		template.Spec.Pod.PodTemplateSpec.ObjectMeta.Annotations = map[string]string{
			constants.GetBackupCommandAnnotation(): template.Spec.BackupCommand,
			constants.GetFileExtensionAnnotation(): template.Spec.FileExtension,
		}

		podLabels := map[string]string{
			"k8up.syn.tools/backupCommandPod": "true",
			"k8up.syn.tools/preBackupPod":     template.GetName(),
		}

		template.Spec.Pod.PodTemplateSpec.ObjectMeta.Labels = podLabels

		deployment := appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      template.GetName(),
				Namespace: p.Obj.GetMetaObject().GetNamespace(),
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: pointer.Int32Ptr(1),
				Template: template.Spec.Pod.PodTemplateSpec,
				Selector: &metav1.LabelSelector{
					MatchLabels: podLabels,
				},
			},
		}

		err := controllerutil.SetOwnerReference(p.Config.Obj.GetMetaObject(), &deployment, p.Scheme)
		if err != nil {
			p.Config.Log.Error(err, "cannot set the owner reference", "name", p.Config.Obj.GetMetaObject().GetName(), "namespace", p.Config.Obj.GetMetaObject().GetNamespace())
		}

		deployments = append(deployments, deployment)
	}

	return deployments
}

func (p *PreBackup) startAndWaitForReady(deployments []appsv1.Deployment) error {

	clientset, err := p.getClientset()
	if err != nil {
		return fmt.Errorf("could not create pre backup pods: %w", err)
	}

	for _, deployment := range deployments {
		p.Log.Info("starting pre backup pod", "namespace", deployment.GetNamespace(), "name", deployment.GetName())

		// Avoid exportloopref
		deployment := deployment

		err := p.Client.Create(p.CTX, &deployment)
		if err != nil {
			return fmt.Errorf("error creating pre backup pods: %w", err)
		}

		watcher, err := clientset.AppsV1().
			Deployments(deployment.GetNamespace()).
			Watch(p.CTX, metav1.SingleObject(deployment.ObjectMeta))
		if err != nil {
			return fmt.Errorf("could not create watcher: %w", err)
		}

		err = p.waitForReady(watcher)
		if err != nil {
			return fmt.Errorf("error during deployment watch: %w", err)
		}

	}
	return nil
}

func (p *PreBackup) getClientset() (*kubernetes.Clientset, error) {

	kubehome := filepath.Join(homedir.HomeDir(), ".kube", "config")
	var config *rest.Config

	if _, err := os.Stat(kubehome); !os.IsNotExist(err) {
		config, err = clientcmd.BuildConfigFromFlags("", kubehome)
		if err != nil {
			return nil, fmt.Errorf("could not load configuration: %s", err)
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("error loading kubernetes configuration inside cluster, check app is running outside kubernetes cluster or run in development mode: %s", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not create clientset: %w", err)
	}

	return clientset, nil
}

func (p *PreBackup) waitForReady(watcher watch.Interface) error {

	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		deployment := event.Object.(*appsv1.Deployment)

		switch event.Type {
		case watch.Modified:

			last := p.getLastDeploymentCondition(deployment)

			if last != nil {
				// if the deadline can't be respected https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#progress-deadline-seconds
				if last.Type == "Progressing" && last.Status == "False" && last.Reason == "ProgressDeadlineExceeded" {
					watcher.Stop()
					return fmt.Errorf("error starting pre backup pod %v: %v", deployment.GetName(), last.Message)
				}
			}

			// Wait until at least one replica is available and continue
			if deployment.Status.AvailableReplicas > 0 {
				return nil
			}

			p.Log.Info("waiting for command pod to get ready", "name", deployment.GetName(), "namespace", deployment.GetNamespace())

		case watch.Error:

			last := p.getLastDeploymentCondition(deployment)

			if last != nil {
				return fmt.Errorf("there was an error while starting pre backup pod %v: %v", deployment.GetName(), last.Message)

			}
			return fmt.Errorf("there was an unknown error while starting pre backup pod %v", deployment.GetName())

		default:
			p.Log.Info("unexpected event during deployment wathc ", "name", deployment.GetName(), "event type", event.Type, "namespace", deployment.GetNamespace())
		}
	}

	return nil
}

func (p *PreBackup) getLastDeploymentCondition(deployment *appsv1.Deployment) *appsv1.DeploymentCondition {
	conditions := deployment.Status.Conditions

	if len(conditions) > 0 {
		return &conditions[len(conditions)-1]
	}
	return nil
}

// Stop will remove the deployments.
func (p *PreBackup) Stop() {

	templates, err := p.getPodTemplates()
	if err != nil {
		p.Log.Error(err, "could not fetch podtemplates", "name", p.Obj.GetMetaObject().GetName(), "namespace", p.Obj.GetMetaObject().GetNamespace())
	}

	deployments := p.generateDeployments(templates)

	namespace := p.Obj.GetMetaObject().GetNamespace()
	option := metav1.DeletePropagationForeground

	for _, deployment := range deployments {
		// Avoid exportloopref
		deployment := deployment
		p.Log.Info("removing prebackup pod", "name", deployment.GetName(), "namespace", namespace)
		err := p.Client.Delete(p.CTX, &deployment, &client.DeleteOptions{
			PropagationPolicy: &option,
		})
		if err != nil {
			if !errors.IsNotFound(err) {
				p.Log.Error(err, "could not create deployment", "name", p.Obj.GetMetaObject().GetName(), "namespace", p.Obj.GetMetaObject().GetNamespace())
			}
		}
	}
}