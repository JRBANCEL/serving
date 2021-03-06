// +build e2e

/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"knative.dev/pkg/ptr"
	pkgTest "knative.dev/pkg/test"
	pkgtest "knative.dev/pkg/test"
	"knative.dev/pkg/test/logstream"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	rnames "knative.dev/serving/pkg/reconciler/revision/resources/names"
	"knative.dev/serving/test"
	v1test "knative.dev/serving/test/v1"
)

// TestActivatorOverload makes sure that activator can handle the load when scaling from 0.
// We need to add a similar test for the User pod overload once the second part of overload handling is done.
func TestActivatorOverload(t *testing.T) {
	t.Parallel()
	cancel := logstream.Start(t)
	defer cancel()

	const (
		// The number of concurrent requests to hit the activator with.
		concurrency = 100
		// How long the service will process the request in ms.
		serviceSleep = 300
	)

	clients := Setup(t)
	names := test.ResourceNames{
		Service: test.ObjectNameForTest(t),
		Image:   "timeout",
	}

	test.CleanupOnInterrupt(func() { test.TearDown(clients, names) })
	defer test.TearDown(clients, names)

	t.Log("Creating a service with run latest configuration.")
	// Create a service with concurrency 1 that sleeps for N ms.
	// Limit its maxScale to 10 containers, wait for the service to scale down and hit it with concurrent requests.
	resources, err := v1test.CreateServiceReady(t, clients, &names,
		func(service *v1.Service) {
			service.Spec.Template.Spec.ContainerConcurrency = ptr.Int64(1)
			service.Spec.Template.Annotations = map[string]string{"autoscaling.knative.dev/maxScale": "10"}
		})
	if err != nil {
		t.Fatalf("Unable to create resources: %v", err)
	}

	// Make sure the service responds correctly before scaling to 0.
	if _, err := pkgTest.WaitForEndpointState(
		clients.KubeClient,
		t.Logf,
		resources.Route.Status.URL.URL(),
		v1test.RetryingRouteInconsistency(pkgtest.IsStatusOK),
		"WaitForSuccessfulResponse",
		test.ServingFlags.ResolvableDomain,
		test.AddRootCAtoTransport(t.Logf, clients, test.ServingFlags.Https),
	); err != nil {
		t.Fatalf("Error probing %s: %v", resources.Route.Status.URL.URL(), err)
	}

	deploymentName := rnames.Deployment(resources.Revision)
	if err := WaitForScaleToZero(t, deploymentName, clients); err != nil {
		t.Fatalf("Unable to observe the Deployment named %s scaling down: %v", deploymentName, err)
	}

	domain := resources.Route.Status.URL.Host
	client, err := pkgTest.NewSpoofingClient(clients.KubeClient, t.Logf, domain, test.ServingFlags.ResolvableDomain, test.AddRootCAtoTransport(t.Logf, clients, test.ServingFlags.Https))
	if err != nil {
		t.Fatalf("Error creating the Spoofing client: %v", err)
	}

	url := fmt.Sprintf("http://%s/?timeout=%d", domain, serviceSleep)

	t.Log("Starting to send out the requests")

	var group sync.WaitGroup
	// Send requests async and wait for the responses.
	for i := 0; i < concurrency; i++ {
		group.Add(1)
		go func() {
			defer group.Done()

			// We need to create a new request per HTTP request because
			// the spoofing client mutates them.
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Errorf("error creating http request: %v", err)
			}

			res, err := client.Do(req)
			if err != nil {
				t.Errorf("unexpected error sending a request, %v", err)
				return
			}

			if res.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want: %d, response: %s", res.StatusCode, http.StatusOK, res)
			}
		}()
	}
	group.Wait()
}
