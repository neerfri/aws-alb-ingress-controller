package ls

import (
	"fmt"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albrgt"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/k8s"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/rs"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tg"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albelbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/annotations/loadbalancer"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/log"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type NewDesiredListenerOptions struct {
	ExistingListener *Listener
	Port             loadbalancer.PortData
	CertificateArn   *string
	Logger           *log.Logger
	SslPolicy        *string
	Ingress          *extensions.Ingress
	Store            store.Storer
	TargetGroups     tg.TargetGroups
	IgnoreHostHeader *bool
}

// NewDesiredListener returns a new listener.Listener based on the parameters provided.
func NewDesiredListener(o *NewDesiredListenerOptions) (*Listener, error) {
	l := &elbv2.Listener{
		Port:     aws.Int64(o.Port.Port),
		Protocol: aws.String(elbv2.ProtocolEnumHttp),
		DefaultActions: []*elbv2.Action{
			{
				Type: aws.String(elbv2.ActionTypeEnumForward),
			},
		},
	}

	if o.CertificateArn != nil && o.Port.Scheme == elbv2.ProtocolEnumHttps {
		l.Certificates = []*elbv2.Certificate{
			{CertificateArn: o.CertificateArn},
		}
		l.Protocol = aws.String(elbv2.ProtocolEnumHttps)
	}

	if o.SslPolicy != nil && o.Port.Scheme == elbv2.ProtocolEnumHttps {
		l.SslPolicy = o.SslPolicy
	}

	listener := &Listener{
		ls:             ls{desired: l},
		logger:         o.Logger,
		defaultBackend: o.Ingress.Spec.Backend,
	}

	if o.ExistingListener != nil {
		listener.rules = o.ExistingListener.rules
	}

	var p int
	for _, rule := range o.Ingress.Spec.Rules {
		var err error

		listener.rules, p, err = rs.NewDesiredRules(&rs.NewDesiredRulesOptions{
			Ingress:          o.Ingress,
			Store:            o.Store,
			Priority:         p,
			Logger:           o.Logger,
			ListenerRules:    listener.rules,
			Rule:             &rule,
			IgnoreHostHeader: o.IgnoreHostHeader,
			TargetGroups:     o.TargetGroups,
		})
		if err != nil {
			return nil, err
		}
	}

	if o.ExistingListener != nil {
		o.ExistingListener.ls.desired = listener.ls.desired
		o.ExistingListener.rules = listener.rules
		o.ExistingListener.defaultBackend = listener.defaultBackend
		return o.ExistingListener, nil
	}

	return listener, nil
}

type NewCurrentListenerOptions struct {
	Listener     *elbv2.Listener
	Logger       *log.Logger
	TargetGroups tg.TargetGroups
}

// NewCurrentListener returns a new listener.Listener based on an elbv2.Listener.
func NewCurrentListener(o *NewCurrentListenerOptions) (*Listener, error) {
	rules, err := rs.NewCurrentRules(&rs.NewCurrentRulesOptions{
		ListenerArn:  o.Listener.ListenerArn,
		Logger:       o.Logger,
		TargetGroups: o.TargetGroups,
	})
	if err != nil {
		return nil, err
	}

	resourceTags, err := albrgt.RGTsvc.GetClusterResources()
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS tags. Error: %s", err.Error())
	}

	var defaultBackend *extensions.IngressBackend
	if *o.Listener.DefaultActions[0].Type == elbv2.ActionTypeEnumForward {
		tgArn := *o.Listener.DefaultActions[0].TargetGroupArn

		tgTags, ok := resourceTags.TargetGroups[tgArn]
		if !ok {
			return nil, fmt.Errorf("TargetGroup %v does not exist in tag map", tgArn)
		}

		svcName, svcPort, err := tgTags.ServiceNameAndPort()
		if err != nil {
			return nil, fmt.Errorf("The Target Group %s does not have the proper tags, can't import: %s", tgArn, err.Error())
		}

		defaultBackend = &extensions.IngressBackend{
			ServiceName: svcName,
			ServicePort: svcPort,
		}
	} else {
		defaultBackend = &extensions.IngressBackend{
			ServicePort: intstr.FromString("use-annotation"),
		}
	}

	return &Listener{
		ls:             ls{current: o.Listener},
		logger:         o.Logger,
		defaultBackend: defaultBackend,
		rules:          rules,
	}, nil
}

// Reconcile compares the current and desired state of this Listener instance. Comparison
// results in no action, the creation, the deletion, or the modification of an AWS listener to
// satisfy the ingress's current state.
func (l *Listener) Reconcile(rOpts *ReconcileOptions) error {
	// If there is a desired listener, set some of the ARNs which are not available when we assemble the desired state
	if l.ls.desired != nil {
		l.ls.desired.LoadBalancerArn = rOpts.LoadBalancerArn

		if l.defaultBackend.ServicePort.String() == "use-annotation" {
			annos, err := rOpts.Store.GetIngressAnnotations(k8s.MetaNamespaceKey(rOpts.Ingress))
			if err != nil {
				return err
			}

			ruleConfig, ok := annos.Action.Actions[l.defaultBackend.ServiceName]
			if !ok {
				return fmt.Errorf("`servicePort: use-annotation` was requested for"+
					"`serviceName: %v` but an annotation for that action does not exist", l.defaultBackend.ServiceName)
			}

			l.ls.desired.DefaultActions[0] = ruleConfig

		} else {
			i := rOpts.TargetGroups.LookupByBackend(*l.defaultBackend)
			if i < 0 {
				return fmt.Errorf("Cannot reconcile listeners, unable to find a target group for default backend %s", l.defaultBackend.String())
			}
			l.ls.desired.DefaultActions[0].TargetGroupArn = rOpts.TargetGroups[i].CurrentARN()
		}
	}

	switch {
	case l.ls.desired == nil: // listener should be deleted
		if l.ls.current == nil {
			break
		}
		l.logger.Infof("Start Listener deletion.")
		if err := l.delete(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "DELETE", "%v listener deleted", *l.ls.current.Port)
		l.logger.Infof("Completed Listener deletion.")

	case l.ls.current == nil && l.ls.desired != nil: // listener doesn't exist and should be created
		l.logger.Infof("Start Listener creation.")
		if err := l.create(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "CREATE", "%v listener created", *l.ls.current.Port)
		l.logger.Infof("Completed Listener creation. ARN: %s | Port: %v | Proto: %s.",
			*l.ls.current.ListenerArn, *l.ls.current.Port,
			*l.ls.current.Protocol)

	case l.needsModification(rOpts): // current and desired diff; needs mod
		if err := l.modify(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "MODIFY", "%v listener modified", *l.ls.current.Port)
	}

	if l.ls.current != nil {
		if rs, err := l.rules.Reconcile(&rs.ReconcileOptions{
			Eventf:       rOpts.Eventf,
			ListenerArn:  l.ls.current.ListenerArn,
			TargetGroups: rOpts.TargetGroups,
		}); err != nil {
			return err
		} else {
			l.rules = rs
		}
	}

	return nil
}

// Adds a Listener to an existing ALB in AWS. This Listener maps the ALB to an existing TargetGroup.
func (l *Listener) create(rOpts *ReconcileOptions) error {
	// Attempt listener creation.
	desired := l.ls.desired
	in := &elbv2.CreateListenerInput{
		Certificates:    desired.Certificates,
		LoadBalancerArn: desired.LoadBalancerArn,
		Protocol:        desired.Protocol,
		Port:            desired.Port,
		SslPolicy:       desired.SslPolicy,
		DefaultActions:  desired.DefaultActions,
	}
	o, err := albelbv2.ELBV2svc.CreateListener(in)
	if err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error creating %v listener: %s", *desired.Port, err.Error())
		return fmt.Errorf("Failed Listener creation: %s.", err.Error())
	}

	l.ls.current = o.Listeners[0]
	return nil
}

// Modifies a listener
func (l *Listener) modify(rOpts *ReconcileOptions) error {
	desired := l.ls.desired
	in := &elbv2.ModifyListenerInput{
		ListenerArn:    l.ls.current.ListenerArn,
		Certificates:   desired.Certificates,
		Port:           desired.Port,
		Protocol:       desired.Protocol,
		SslPolicy:      desired.SslPolicy,
		DefaultActions: desired.DefaultActions,
	}

	o, err := albelbv2.ELBV2svc.ModifyListener(in)
	if err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error modifying %v listener: %s", *desired.Port, err.Error())
		return fmt.Errorf("Failed Listener modification: %s", err.Error())
	}
	l.ls.current = o.Listeners[0]

	return nil
}

// delete removes a Listener from an existing ALB in AWS.
func (l *Listener) delete(rOpts *ReconcileOptions) error {
	if err := albelbv2.ELBV2svc.RemoveListener(l.ls.current.ListenerArn); err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error deleting %v listener: %s", *l.ls.current.Port, err.Error())
		return fmt.Errorf("Failed Listener deletion. ARN: %s: %s", *l.ls.current.ListenerArn, err.Error())
	}

	l.deleted = true
	return nil
}

// needsModification returns true when the current and desired listener state are not the same.
// representing that a modification to the listener should be attempted.
func (l *Listener) needsModification(rOpts *ReconcileOptions) bool {
	lsc := l.ls.current
	lsd := l.ls.desired

	switch {
	case lsc == nil && lsd == nil:
		return false
	case lsc == nil:
		l.logger.Debugf("Current is nil")
		return true
	case !util.DeepEqual(lsc.Port, lsd.Port):
		l.logger.Debugf("Port needs to be changed (%v != %v)", log.Prettify(lsc.Port), log.Prettify(lsd.Port))
		return true
	case !util.DeepEqual(lsc.Protocol, lsd.Protocol):
		l.logger.Debugf("Protocol needs to be changed (%v != %v)", log.Prettify(lsc.Protocol), log.Prettify(lsd.Protocol))
		return true
	case !util.DeepEqual(lsc.Certificates, lsd.Certificates):
		l.logger.Debugf("Certificates needs to be changed (%v != %v)", log.Prettify(lsc.Certificates), log.Prettify(lsd.Certificates))
		return true
	case !util.DeepEqual(lsc.DefaultActions, lsd.DefaultActions):
		l.logger.Debugf("DefaultActions needs to be changed (%v != %v)", log.Prettify(lsc.DefaultActions), log.Prettify(lsd.DefaultActions))
		return true
	case !util.DeepEqual(lsc.SslPolicy, lsd.SslPolicy):
		l.logger.Debugf("SslPolicy needs to be changed (%v != %v)", log.Prettify(lsc.SslPolicy), log.Prettify(lsd.SslPolicy))
		return true
	}
	return false
}

// StripDesiredState removes the desired state from the listener.
func (l *Listener) StripDesiredState() {
	l.ls.desired = nil
	l.rules.StripDesiredState()
}

// stripCurrentState removes the current state from the listener.
func (l *Listener) stripCurrentState() {
	l.ls.current = nil
	l.rules.StripCurrentState()
}

func (l *Listener) GetRules() rs.Rules {
	return l.rules
}

func (l *Listener) DefaultActionArn() *string {
	if *l.ls.current.DefaultActions[0].Type == elbv2.ActionTypeEnumRedirect {
		return l.ls.current.DefaultActions[0].TargetGroupArn
	}
	return nil
}
