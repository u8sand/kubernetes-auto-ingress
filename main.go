package main

import (
    "os"
    "time"
    "flag"
    "reflect"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/tools/clientcmd"
    core "k8s.io/api/core/v1"
    extensions "k8s.io/api/extensions/v1beta1"
    "k8s.io/apimachinery/pkg/fields"
    "k8s.io/apimachinery/pkg/util/intstr"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

    //comment if not using gcp
    _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

    log "github.com/Sirupsen/logrus"
)

var (
    //dns wildcard record for all applications created, should be like example.com
    wildcardRecord = os.Getenv("AUTO_INGRESS_SERVER_NAME")
    //secret for ssl/tls of namespace where auto-ingress is running
    secret = os.Getenv("AUTO_INGRESS_SECRET")
    //read kubeconfig
    kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
)

func main() {
    flag.Parse()

    var err error
    var config *rest.Config

    //if kubeconfig is specified, use out-of-cluster
    if *kubeconfig != "" {
        config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
    } else {
        //get config when running inside Kubernetes
        config, err = rest.InClusterConfig()
    }

    if err != nil {
        log.Errorln(err.Error())
        return
    }

    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        log.Errorln(err.Error())
        return
    }

    //map to keep track of which services have been already auto-ingressed
	var svcIngPair map[string]extensions.Ingress
    svcIngPair = make(map[string]extensions.Ingress)

    //get current ingresses on cluster
    log.Info("Initializing mapping between ingresses and services...")
	err = createIngressServiceMap(clientset, svcIngPair)
    if err != nil {
        log.Errorln(err.Error())
        return
    }

    log.Info("Initialized map: ", reflect.ValueOf(svcIngPair).MapKeys())

    //create a watch to listen for create/update/delete event on service
    //new created service will be auto-ingressed if it specifies label "autoingress: true"
    //deleted service will be remove the associated ingress if it specifies label "autoingress: true"
    watchlist := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "services", "",
        fields.Everything())
    _, controller := cache.NewInformer(
        watchlist,
        &core.Service{},
        time.Second * 0,
        cache.ResourceEventHandlerFuncs{
            AddFunc: func(obj interface{}) {
                svc := obj.(*core.Service)
                log.Info("Service added: ", svc.Name)
                lb := svc.Labels
                if _, found1 := svcIngPair[svc.Name]; !found1 {
                    if val, found2 := lb["public"]; found2 {
                        if val == "true" {
                            newIng, err := createIngressForService(clientset, *svc)
                            if err != nil {
                                log.Errorln(err.Error())
                            } else {
                                log.Info("Created new ingress for service: ", svc.Name)
                                svcIngPair[svc.Name] = *newIng
                                log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
                            }
                        }
                    }
                }
            },
            DeleteFunc: func(obj interface{}) {
                svc := obj.(*core.Service)
                log.Info("Service deleted: ", svc.Name)
				if ing, found := svcIngPair[svc.Name]; found {
					clientset.ExtensionsV1beta1().Ingresses(svc.Namespace).Delete(ing.Name, nil)
					log.Info("Deleted ingress for service: ", svc.Name)
                    delete(svcIngPair, svc.Name)
                    log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
				}
            },
            UpdateFunc:func(oldObj, newObj interface{}) {
                newSvc := newObj.(*core.Service)
                log.Info("Service changed: ", newSvc.Name)
                lb := newSvc.Labels
                if ing, found1 := svcIngPair[newSvc.Name]; found1 {
                    if val, found2 := lb["public"]; !found2 {
                        clientset.ExtensionsV1beta1().Ingresses(newSvc.Namespace).Delete(ing.Name, nil)
                        log.Info("Deleted ingress for service: ", newSvc.Name)
                        delete(svcIngPair, newSvc.Name)
                        log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
                    } else {
                        if val == "false" {
                            clientset.ExtensionsV1beta1().Ingresses(newSvc.Namespace).Delete(ing.Name, nil)
                            log.Info("Deleted ingress for service: ", newSvc.Name)
                            delete(svcIngPair, newSvc.Name)
                            log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
                        }
                    }
                } else {
                    if val, found3 := lb["public"]; found3 {
                        if val == "true" {
                            newIng, err := createIngressForService(clientset, *newSvc)
                            if err != nil {
                                log.Errorln(err.Error())
                            } else {
                                log.Info("created new ingress for service: ", newSvc.Name)
                                svcIngPair[newSvc.Name] = *newIng
                                log.Info("Updated map: ", reflect.ValueOf(svcIngPair).MapKeys())
                            }
                        }
                    }
                }
            },
        },
    )
    stop := make(chan struct{})
    go controller.Run(stop)
    for{
        time.Sleep(time.Second)
    }
}

//create service map in the initial phase to check the current ingresses running on cluster
func createIngressServiceMap(clientset *kubernetes.Clientset, m map[string]extensions.Ingress) error {

	services, err := clientset.CoreV1().Services("").List(metav1.ListOptions{})

    if err != nil {
        return err
    }

	//get ingresses from all namespaces
    ingresses, err:= clientset.ExtensionsV1beta1().Ingresses("").List(metav1.ListOptions{})

    if err != nil {
        return err
    }

	//get all services which have "public" labels and their associated ingresses
    for i:=0; i < len(ingresses.Items); i++ {
        rules := ingresses.Items[i].Spec.Rules
        for j:=0; j < len(rules); j++ {
            paths := rules[j].HTTP.Paths
            for k:=0; k < len(paths); k++ {
                svcName := paths[k].Backend.ServiceName
                if _, found := m[svcName]; !found {
                    m[svcName] = ingresses.Items[i]
                }
            }
        }
    }

	//if there is any services with the label "public" but haven't had the ingresses, create them.
	for i:=0; i < len(services.Items); i++ {
		lb := services.Items[i].GetLabels()
		svcName := services.Items[i].GetName()
        if _, found1 := m[svcName]; !found1 {
            if val, found2 := lb["public"]; found2 {
                if val == "true" {
                    newIng, err := createIngressForService(clientset, services.Items[i])
                    if err != nil {
                        return err
                    }
                    m[services.Items[i].GetName()] = *newIng
                }
            }
        } else {
			if val, found2 := lb["public"]; found2 {
				if val == "false" {
					delete(m, svcName)
				}
			} else {
				delete(m, svcName)
			}
		}
    }

    return nil
}

//create an ingress for the associated service
func createIngressForService(clientset *kubernetes.Clientset, service core.Service) (*extensions.Ingress, error) {
	backend := createIngressBackend(service)

    ingress := createIngress(service, backend)

    newIng, err := clientset.ExtensionsV1beta1().Ingresses(service.Namespace).Create(ingress)

    return newIng, err
}

//create an ingress backend before putting it to the ingress
func createIngressBackend(service core.Service) extensions.IngressBackend {
    serviceName := service.GetName()
    if len(service.Spec.Ports) > 0 {
        var servicePort32 interface{}
        var servicePort int

        servicePort32 = service.Spec.Ports[0].Port
        servicePort32Tmp := servicePort32.(int32)

        servicePort = int(servicePort32Tmp)
        return extensions.IngressBackend {
            ServiceName: serviceName,
            ServicePort: intstr.FromInt(servicePort),
        }
    }

    return extensions.IngressBackend {}
}

//create ingress for associated service
func createIngress(service core.Service, backend extensions.IngressBackend) *extensions.Ingress {

    ingressname := service.Name
    servername := ingressname + "." + wildcardRecord

    return &extensions.Ingress {
        ObjectMeta: metav1.ObjectMeta {
            Name: ingressname,
            Namespace: service.Namespace,
        },
        Spec: extensions.IngressSpec {
            TLS: []extensions.IngressTLS{
                {
                    Hosts: []string{
                        servername,
                    },
                    SecretName: secret,

                },
            },
            Rules: []extensions.IngressRule {
                {
                    Host: servername,
                    IngressRuleValue: extensions.IngressRuleValue {
                        HTTP: &extensions.HTTPIngressRuleValue {
                        Paths: []extensions.HTTPIngressPath {
                            {
                                Path: "/",
                                Backend: backend,
                            },
                        },
                        },
                    },
                },
            },
        },
    }
}
