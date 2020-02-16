package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/kubernetes-sigs/aws-iam-authenticator/pkg/token"

	apiv1batch "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type EventBridgeEvent struct {
	Id         string    `json:"id"`
	Time       time.Time `json:"time"`
	DetailType string    `json:"detail-type"`
	Detail     struct {
		Job struct {
			Uuid            string   `json:"uuid"`
			AgentQueryRules []string `json:"agent_query_rules,omitempty"`
		} `json:"job"`
		Build struct {
			Id     json.Number `json:"number,Number"`
			Commit string      `json:"commit"`
		} `json:"build"`
		Pipeline struct {
			Slug string `json:"slug"`
		} `json:"pipeline"`
		Organization struct {
			Slug string `json:"slug"`
		} `json:"organization"`
	} `json:"detail"`
}

type Config struct {
	Cluster        string
	Region         string
	Arn            string
	BuildkiteToken string
	Namespace      string
	PodPrefix      string
	Image          string
}

type Cluster struct {
	Name     string
	Endpoint string
	CA       []byte
	Role     string
}

var config Config

/* Begin utilities */

func createInt64Pointer(x int64) *int64 {
	return &x
}

func createInt32Pointer(x int32) *int32 {
	return &x
}

/* End utilities */

func populateConfig() Config {
	var newConfig Config
	newConfig.Cluster = os.Getenv("cluster")
	if len(newConfig.Cluster) < 1 {
		panic("Unable to grab cluster name from ENVIRONMENT variable")
	}

	newConfig.Arn = os.Getenv("arn")
	if len(newConfig.Arn) < 1 {
		panic("Unable to grab role ARN from ENVIRONMENT variable")
	}

	newConfig.Region = os.Getenv("region")
	if len(newConfig.Arn) < 1 {
		// Lambda exposes the region the function is running in, use that as a fallback
		newConfig.Arn = os.Getenv("AWS_REGION")
	}

	newConfig.BuildkiteToken = os.Getenv("buildkite_token")
	if len(newConfig.BuildkiteToken) < 1 {
		panic("Unable to grab buildkite token from ENVIRONMENT variable")
	}

	newConfig.Namespace = os.Getenv("namespace")
	if len(newConfig.Namespace) < 1 {
		// Configurable, but default to "buildkite"
		newConfig.Namespace = "buildkite"
	}

	newConfig.PodPrefix = os.Getenv("pod_prefix")
	if len(newConfig.PodPrefix) < 1 {
		// Configurable, but default to "buildkite-agent-"
		newConfig.PodPrefix = "buildkite-agent-"
	}

	newConfig.Image = os.Getenv("image")
	if len(newConfig.Image) < 1 {
		// Configurable, but default to "buildkite/agent"
		newConfig.PodPrefix = "buildkite/agent"
	}

	return newConfig
}

func getClusterDetails() (*Cluster, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String(config.Region)})
	svc := eks.New(sess)
	input := &eks.DescribeClusterInput{
		Name: aws.String(config.Cluster),
	}

	result, err := svc.DescribeCluster(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case eks.ErrCodeResourceNotFoundException:
				fmt.Println(eks.ErrCodeResourceNotFoundException, aerr.Error())
			case eks.ErrCodeClientException:
				fmt.Println(eks.ErrCodeClientException, aerr.Error())
			case eks.ErrCodeServerException:
				fmt.Println(eks.ErrCodeServerException, aerr.Error())
			case eks.ErrCodeServiceUnavailableException:
				fmt.Println(eks.ErrCodeServiceUnavailableException, aerr.Error())
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			fmt.Println(err.Error())
		}
		return nil, err
	}

	// The CA data comes base64 encoded string inside a JSON object { "Data": "..." }
	ca, err := base64.StdEncoding.DecodeString(*result.Cluster.CertificateAuthority.Data)
	if err != nil {
		return nil, err
	}

	return &Cluster{
		Name:     *result.Cluster.Name,
		Endpoint: *result.Cluster.Endpoint,
		CA:       ca,
		Role:     config.Arn,
	}, nil
}

func (c *Cluster) authToken() (string, error) {
	// Init a new aws-iam-authenticator token generator
	gen, err := token.NewGenerator(false)
	if err != nil {
		return "", err
	}

	// Use the current IAM credentials to obtain a K8s bearer token
	tok, err := gen.GetWithRole(c.Name, c.Role)
	if err != nil {
		return "", err
	}

	return tok, nil
}

func checkAgentExistence(job string, clientset *kubernetes.Clientset) bool {
	jobs, err := clientset.BatchV1().Jobs(config.Namespace).List(metav1.ListOptions{LabelSelector: "buildkite-queue=" + job})
	if err != nil {
		panic("Error fetching existing k8s jobs, " + err.Error())
	}

	if len(jobs.Items) > 0 {
		// job found with buildkite-queue label = the uuid we're looking for
		fmt.Printf("Found k8s job for %s, %v", job, jobs.Items)
		return true
	} else {
		fmt.Printf("No k8s job found for %s", job)
		return false
	}
}

func createBuildAgent(job string, clientset *kubernetes.Clientset) error {
	t := true

	buildAgent := &apiv1batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.PodPrefix + job,
			Labels: map[string]string{
				"buildkite-job-id": job,
			},
		},
		Spec: apiv1batch.JobSpec{
			ActiveDeadlineSeconds:   createInt64Pointer(3600), // Allow a job to run for 60 minutes at most, accounting for 1 pod running for 30 minutes and failing + 1 retry
			BackoffLimit:            createInt32Pointer(1),
			TTLSecondsAfterFinished: createInt32Pointer(600),
			Template: apiv1.PodTemplateSpec{
				Spec: apiv1.PodSpec{
					Tolerations: []apiv1.Toleration{
						{
							Key:      "workloads",
							Operator: "Equal",
							Value:    "buildkite",
							Effect:   "NoSchedule",
						},
					},
					NodeSelector: map[string]string{
						"workloads": "buildkite",
					},
					RestartPolicy:                 apiv1.RestartPolicyNever,
					ActiveDeadlineSeconds:         createInt64Pointer(1800), // Allow an agent to run for 30 minutes
					TerminationGracePeriodSeconds: createInt64Pointer(600),
					InitContainers: []apiv1.Container{
						{
							Name:            "buildkite-agent-exposer",
							Image:           config.Image,
							ImagePullPolicy: "Always",
							Args: []string{
								"cp /usr/local/bin/buildkite-agent /buildkite-agent/buildkite-agent",
							},
							Command: []string{
								"/bin/sh",
								"-c",
							},
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      "buildkite-agent",
									MountPath: "/buildkite-agent",
								},
							},
						},
					},
					Containers: []apiv1.Container{
						{
							Name:            "buildkite-agent",
							Image:           config.Image,
							ImagePullPolicy: "Always",
							SecurityContext: &apiv1.SecurityContext{Privileged: &t},
							Env: []apiv1.EnvVar{
								{
									Name:  "BUILDKITE_AGENT_ACQUIRE_JOB",
									Value: job,
								},
								{
									Name:  "DOCKER_HOST",
									Value: "tcp://localhost:2375",
								},
								{
									Name: "BUILDKITE_AGENT_TOKEN",
									ValueFrom: &apiv1.EnvVarSource{
										SecretKeyRef: &apiv1.SecretKeySelector{
											LocalObjectReference: apiv1.LocalObjectReference{Name: "buildkite-agent-token"},
											Key:                  "token",
										},
									},
								},
								{
									Name: "GITHUB_TOKEN",
									ValueFrom: &apiv1.EnvVarSource{
										SecretKeyRef: &apiv1.SecretKeySelector{
											LocalObjectReference: apiv1.LocalObjectReference{Name: "buildkite-env-vars"},
											Key:                  "GITHUB_TOKEN",
										},
									},
								},
								{
									Name: "DOCKER_LOGIN_USER",
									ValueFrom: &apiv1.EnvVarSource{
										SecretKeyRef: &apiv1.SecretKeySelector{
											LocalObjectReference: apiv1.LocalObjectReference{Name: "buildkite-env-vars"},
											Key:                  "DOCKER_LOGIN_USER",
										},
									},
								},
								{
									Name: "DOCKER_LOGIN_PASSWORD",
									ValueFrom: &apiv1.EnvVarSource{
										SecretKeyRef: &apiv1.SecretKeySelector{
											LocalObjectReference: apiv1.LocalObjectReference{Name: "buildkite-env-vars"},
											Key:                  "DOCKER_LOGIN_PASSWORD",
										},
									},
								},
							},
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      "git-credentials",
									MountPath: "/root/.git-credentials",
									SubPath:   ".git-credentials",
								},
								{
									Name:      "buildkite-agent",
									MountPath: "/buildkite-agent",
								},
								{
									Name:      "buildkite-builds",
									MountPath: "/buildkite-builds",
								},
							},
						},
						{
							Name:            "docker-dind",
							Image:           "docker:18.09-dind",
							ImagePullPolicy: "Always",
							SecurityContext: &apiv1.SecurityContext{Privileged: &t},
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      "buildkite-agent",
									MountPath: "/buildkite-agent",
								},
								{
									Name:      "buildkite-builds",
									MountPath: "/buildkite-builds",
								},
							},
							Resources: apiv1.ResourceRequirements{
								Requests: apiv1.ResourceList{
									apiv1.ResourceCPU:    *resource.NewMilliQuantity(1250, resource.DecimalSI),
									apiv1.ResourceMemory: *resource.NewQuantity(6144, resource.DecimalSI),
								},
							},
						},
					},
					Volumes: []apiv1.Volume{
						{
							Name: "buildkite-builds",
							VolumeSource: apiv1.VolumeSource{
								HostPath: &apiv1.HostPathVolumeSource{Path: "/buildkite/builds"},
							},
						},
						{
							Name: "buildkite-agent",
							VolumeSource: apiv1.VolumeSource{
								EmptyDir: &apiv1.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "git-credentials",
							VolumeSource: apiv1.VolumeSource{
								Secret: &apiv1.SecretVolumeSource{SecretName: "buildkite-agent-git-credentials", DefaultMode: createInt32Pointer(256)},
							},
						},
					},
				},
			},
		},
	}

	_, err := clientset.BatchV1().Jobs(config.Namespace).Create(buildAgent)
	if err != nil {
		panic("Error creating k8s job, " + err.Error())
	}

	fmt.Printf("Created agent k8s job for job %s", job)
	return nil
}

func lambdaHandler(event EventBridgeEvent) {
	config = populateConfig()

	// Get EKS cluster details
	cluster, err := getClusterDetails()
	fmt.Printf("Amazon EKS Cluster: %s (%s)\n", cluster.Name, cluster.Endpoint)

	// Use the aws-iam-authenticator to fetch a K8s authentication bearer token
	token, err := cluster.authToken()
	if err != nil {
		panic("Failed to obtain token from aws-iam-authenticator, " + err.Error())
	}

	// Create a new K8s client set using the Amazon EKS cluster details
	clientset, err := kubernetes.NewForConfig(&rest.Config{
		Host:        cluster.Endpoint,
		BearerToken: token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: cluster.CA,
		},
	})
	if err != nil {
		panic("Failed to create new k8s client, " + err.Error())
	}

	// Check whether this Job event if for an agent we need to create
	if event.Detail.Job.AgentQueryRules[0] == "queue=dynamic" {
		// Verify that an agent doesn't already exist
		if checkAgentExistence(event.Detail.Job.Uuid, clientset) != true {
			// Create the agent
			createBuildAgent(event.Detail.Job.Uuid, clientset)
		}
	}
}

func main() {
	lambda.Start(lambdaHandler)
}
