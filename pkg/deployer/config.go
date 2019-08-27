package deployer

import (
	"context"
	"fmt"
	"github.com/armory-io/spinnaker-operator/pkg/halconfig"
	"github.com/armory-io/spinnaker-operator/pkg/util"
	"reflect"
	"strings"
	"time"

	spinnakerv1alpha1 "github.com/armory-io/spinnaker-operator/pkg/apis/spinnaker/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// IsConfigUpToDate returns true if the config in status represents the latest
// config in the service spec
func (d *Deployer) IsConfigUpToDate(svc *spinnakerv1alpha1.SpinnakerService, config runtime.Object, hc *halconfig.SpinnakerConfig) (bool, error) {
	rLogger := d.log.WithValues("Service", svc.Name)
	if !d.isHalconfigUpToDate(svc, config) {
		rLogger.Info("Detected change in Spinnaker configs")
		return false, nil
	}

	upToDate, err := d.isExposeConfigUpToDate(svc, hc)
	if err != nil {
		return false, err
	}
	if !upToDate {
		rLogger.Info("Detected change in expose configuration")
		return false, nil
	}

	return true, nil
}

// isHalconfigUpToDate returns true if the hal config in status represents the latest
// config in the service spec
func (d *Deployer) isHalconfigUpToDate(instance *spinnakerv1alpha1.SpinnakerService, config runtime.Object) bool {
	hcStat := instance.Status.HalConfig
	cm, ok := config.(*corev1.ConfigMap)
	if ok {
		cmStatus := hcStat.ConfigMap
		return cmStatus != nil && cmStatus.Name == cm.ObjectMeta.Name && cmStatus.Namespace == cm.ObjectMeta.Namespace &&
			cmStatus.ResourceVersion == cm.ObjectMeta.ResourceVersion
	}
	sec, ok := config.(*corev1.Secret)
	if ok {
		secStatus := hcStat.Secret
		return secStatus != nil && secStatus.Name == sec.ObjectMeta.Name && secStatus.Namespace == sec.ObjectMeta.Namespace &&
			secStatus.ResourceVersion == sec.ObjectMeta.ResourceVersion
	}
	return false
}

func (d *Deployer) isExposeConfigUpToDate(svc *spinnakerv1alpha1.SpinnakerService, hc *halconfig.SpinnakerConfig) (bool, error) {
	switch strings.ToLower(svc.Spec.Expose.Type) {
	case "":
		return true, nil
	case "service":
		isDeckSSLEnabled, err := hc.GetHalConfigPropBool(util.DeckSSLEnabledProp, false)
		if err != nil {
			isDeckSSLEnabled = false
		}
		upToDateDeck, err := d.isExposeServiceUpToDate(svc, util.DeckServiceName, isDeckSSLEnabled)
		if !upToDateDeck || err != nil {
			return false, err
		}
		isGateSSLEnabled, err := hc.GetHalConfigPropBool(util.GateSSLEnabledProp, false)
		if err != nil {
			isGateSSLEnabled = false
		}
		upToDateGate, err := d.isExposeServiceUpToDate(svc, util.GateServiceName, isGateSSLEnabled)
		if !upToDateGate || err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("expose type %s not supported. Valid types: \"service\"", svc.Spec.Expose.Type)
	}
}

func (d *Deployer) isExposeServiceUpToDate(spinSvc *spinnakerv1alpha1.SpinnakerService, serviceName string, hcSSLEnabled bool) (bool, error) {
	rLogger := d.log.WithValues("Service", spinSvc.Name)
	ns := spinSvc.ObjectMeta.Namespace
	svc, err := util.GetService(serviceName, ns, d.client)
	if err != nil {
		return false, err
	}
	// we need a service to exist, therefore it's not "up to date"
	if svc == nil {
		return false, nil
	}

	// service type is different, redeploy
	if upToDate, err := d.exposeServiceTypeUpToDate(serviceName, spinSvc, svc); !upToDate || err != nil {
		return false, err
	}

	// annotations are different, redeploy
	simpleServiceName := serviceName[len("spin-"):]
	expectedAnnotations := spinSvc.GetAggregatedAnnotations(simpleServiceName)
	if !reflect.DeepEqual(svc.Annotations, expectedAnnotations) {
		rLogger.Info(fmt.Sprintf("Service annotations for %s: expected: %s, actual: %s", serviceName,
			expectedAnnotations, svc.Annotations))
		return false, nil
	}

	// status url is available but not set yet, redeploy
	statusUrl := spinSvc.Status.APIUrl
	if serviceName == "spin-deck" {
		statusUrl = spinSvc.Status.UIUrl
	}
	if statusUrl == "" {
		lbUrl, err := util.FindLoadBalancerUrl(serviceName, ns, d.client, hcSSLEnabled)
		if err != nil {
			return false, err
		}
		if lbUrl != "" {
			rLogger.Info(fmt.Sprintf("Status url of %s is not set and load balancer url is ready", serviceName))
			return false, nil
		}
	}

	return true, nil
}

func (d *Deployer) exposeServiceTypeUpToDate(serviceName string, spinSvc *spinnakerv1alpha1.SpinnakerService, svc *corev1.Service) (bool, error) {
	rLogger := d.log.WithValues("Service", spinSvc.Name)
	formattedServiceName := serviceName[len("spin-"):]
	if c, ok := spinSvc.Spec.Expose.Service.Overrides[formattedServiceName]; ok && c.Type != "" {
		if string(svc.Spec.Type) != c.Type {
			rLogger.Info(fmt.Sprintf("Service type for %s: expected: %s, actual: %s", serviceName,
				c.Type, string(svc.Spec.Type)))
			return false, nil
		}
	} else {
		if string(svc.Spec.Type) != spinSvc.Spec.Expose.Service.Type {
			rLogger.Info(fmt.Sprintf("Service type for %s: expected: %s, actual: %s", serviceName,
				spinSvc.Spec.Expose.Service.Type, string(svc.Spec.Type)))
			return false, nil
		}
	}
	return true, nil
}

func (d *Deployer) commitConfigToStatus(ctx context.Context, svc *spinnakerv1alpha1.SpinnakerService, status *spinnakerv1alpha1.SpinnakerServiceStatus, config runtime.Object) error {
	cm, ok := config.(*corev1.ConfigMap)
	if ok {
		status.HalConfig = spinnakerv1alpha1.SpinnakerFileSourceStatus{
			ConfigMap: &spinnakerv1alpha1.SpinnakerFileSourceReferenceStatus{
				Name:            cm.ObjectMeta.Name,
				Namespace:       cm.ObjectMeta.Namespace,
				ResourceVersion: cm.ObjectMeta.ResourceVersion,
			},
		}
	}
	sec, ok := config.(*corev1.Secret)
	if ok {
		status.HalConfig = spinnakerv1alpha1.SpinnakerFileSourceStatus{
			Secret: &spinnakerv1alpha1.SpinnakerFileSourceReferenceStatus{
				Name:            sec.ObjectMeta.Name,
				Namespace:       sec.ObjectMeta.Namespace,
				ResourceVersion: sec.ObjectMeta.ResourceVersion,
			},
		}
	}
	status.LastConfigurationTime = metav1.NewTime(time.Now())
	// gate and deck status url's are populated in transformers

	s := svc.DeepCopy()
	s.Status = *status
	// Following doesn't work (EKS) - looks like PUTting to the subresource (status) gives a 404
	// TODO Investigate issue on earlier Kubernetes version, works fine in 1.13
	return d.client.Status().Update(ctx, s)
}