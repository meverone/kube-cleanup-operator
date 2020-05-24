package controller

import (
	"context"
	"log"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

func ignoreNotFound(err error) error {
	if apierrs.IsNotFound(err) {
		return nil
	}
	return err
}

const resyncPeriod = time.Second * 30

// Kleaner watches the kubernetes api for changes to Pods and
// delete completed Pods without specific annotation
type Kleaner struct {
	podInformer cache.SharedIndexInformer
	jobInformer cache.SharedIndexInformer
	kclient     *kubernetes.Clientset

	deleteSuccessfulAfter time.Duration
	deleteFailedAfter     time.Duration
	deletePendingAfter    time.Duration
	deleteOrphanedAfter   time.Duration

	dryRun bool
	ctx    context.Context
}

// NewKleaner creates a new NewKleaner
func NewKleaner(ctx context.Context, kclient *kubernetes.Clientset, namespace string, dryRun bool, deleteSuccessfulAfter,
	deleteFailedAfter, deletePendingAfter, deleteOrphanedAfter time.Duration) *Kleaner {
	jobInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return kclient.BatchV1().Jobs(namespace).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return kclient.BatchV1().Jobs(namespace).Watch(ctx, options)
			},
		},
		&batchv1.Job{},
		resyncPeriod,
		cache.Indexers{},
	)
	// Create informer for watching Namespaces
	podInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return kclient.CoreV1().Pods(namespace).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return kclient.CoreV1().Pods(namespace).Watch(ctx, options)
			},
		},
		&corev1.Pod{},
		resyncPeriod,
		cache.Indexers{},
	)
	kleaner := &Kleaner{
		dryRun:                dryRun,
		kclient:               kclient,
		ctx:                   ctx,
		deleteSuccessfulAfter: deleteSuccessfulAfter,
		deleteFailedAfter:     deleteFailedAfter,
		deletePendingAfter:    deletePendingAfter,
		deleteOrphanedAfter:   deleteOrphanedAfter,
	}
	jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			kleaner.Process(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			if !reflect.DeepEqual(old, new) {
				kleaner.Process(new)
			}
		},
	})
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			kleaner.Process(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			if !reflect.DeepEqual(old, new) {
				kleaner.Process(new)
			}
		},
	})

	kleaner.podInformer = podInformer
	kleaner.jobInformer = jobInformer

	return kleaner
}

func (c *Kleaner) periodicCacheCheck() {
	for {
		for _, job := range c.jobInformer.GetStore().List() {
			c.Process(job)
		}
		for _, obj := range c.podInformer.GetStore().List() {
			c.Process(obj)
		}
		time.Sleep(2 * resyncPeriod)
	}
}

// Run starts the process for listening for pod changes and acting upon those changes.
func (c *Kleaner) Run(stopCh <-chan struct{}) {
	log.Printf("Listening for changes...")

	go c.podInformer.Run(stopCh)
	go c.jobInformer.Run(stopCh)

	go c.periodicCacheCheck()

	<-stopCh
}

func (c *Kleaner) Process(obj interface{}) {
	switch t := obj.(type) {
	case *batchv1.Job:
		job := t
		log.Printf("Found a job: %s. completionTime: %v active: %v", job.Name, job.Status.CompletionTime, job.Status.Active)
		// skip the job if it hasn't completed yet or has any active pods
		if job.Status.CompletionTime.IsZero() || job.Status.Active > 0 {
			return
		}
		timeSinceCompletion := time.Now().Sub(job.Status.CompletionTime.Time)
		if job.Status.Succeeded > 0 {
			if c.deleteSuccessfulAfter > 0 && timeSinceCompletion > c.deleteSuccessfulAfter {
				c.deleteJobs(job)
			}
		}
		if job.Status.Failed > 0 {
			if c.deleteFailedAfter > 0 && timeSinceCompletion >= c.deleteFailedAfter {
				c.deleteJobs(job)
			}
		}

	case *corev1.Pod:
		pod := t
		ownedByJob := podOwnedByJob(pod)
		log.Printf("Found a pod: %s. owned by job %v", pod.Name, ownedByJob)
		if !ownedByJob && c.deleteOrphanedAfter == 0 {
			return
		}
		podFinishTime := extractPodFinishTime(pod)
		if podFinishTime.IsZero() {
			return
		}
		age := time.Now().Sub(podFinishTime)
		log.Printf("Found a pod: %s. completionTime: %v age: %v", pod.Name, podFinishTime, age)
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			if c.deleteSuccessfulAfter > 0 && age >= c.deleteSuccessfulAfter {
				c.deletePods(pod)
			}
		case corev1.PodFailed:
			if c.deleteFailedAfter > 0 && age >= c.deleteFailedAfter {
				c.deletePods(pod)
			}
		case corev1.PodPending:
			if c.deletePendingAfter > 0 && age >= c.deletePendingAfter {
				c.deletePods(pod)
			}
		default:
			return
		}
	}
}

func (c *Kleaner) deleteJobs(job *batchv1.Job) {
	if c.dryRun {
		log.Printf("dry-run: Job '%s:%s' would have been deleted", job.Namespace, job.Name)
		return
	}
	log.Printf("Deleting job '%s:%s'", job.Namespace, job.Name)
	propagation := metav1.DeletePropagationForeground
	jo := metav1.DeleteOptions{PropagationPolicy: &propagation}
	if err := c.kclient.BatchV1().Jobs(job.Namespace).Delete(c.ctx, job.Name, jo); ignoreNotFound(err) != nil {
		log.Printf("failed to delete job '%s:%s': %v", job.Namespace, job.Name, err)
	}
}

func (c *Kleaner) deletePods(pod *corev1.Pod) {
	if c.dryRun {
		log.Printf("dry-run: Pod '%s:%s' would have been deleted", pod.Namespace, pod.Name)
	}
	log.Printf("Deleting pod '%s:%s'", pod.Namespace, pod.Name)
	var po metav1.DeleteOptions
	if err := c.kclient.CoreV1().Pods(pod.Namespace).Delete(c.ctx, pod.Name, po); ignoreNotFound(err) != nil {
		log.Printf("failed to delete pod '%s:%s': %v", pod.Namespace, pod.Name, err)
	}
}

func podOwnedByJob(pod *corev1.Pod) bool {
	// Going all over the owners, looking for a job, usually there is only one owner
	for _, ow := range pod.OwnerReferences {
		if ow.Kind == "Job" {
			return true
		}
	}
	return false
}

func extractPodFinishTime(podObj *corev1.Pod) time.Time {
	for _, pc := range podObj.Status.Conditions {
		// Looking for the time when pod's condition "Ready" became "false" (equals end of execution)
		if pc.Type == corev1.PodReady && pc.Status == corev1.ConditionFalse {
			return pc.LastTransitionTime.Time
		}
	}
	return time.Time{}
}
