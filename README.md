# freeipa-operator

An operator for managing FreeIPA identity servers on Kubernetes and OpenShift.

## Description

The FreeIPA Operator automates the deployment and lifecycle management of FreeIPA servers on Kubernetes and OpenShift clusters. It provides a Kubernetes-native way to manage FreeIPA instances using Custom Resources.

## Operator Hub / OpenShift Installation

The operator is distributed as a bundle and catalog image for use with the Operator Lifecycle Manager (OLM).

### Catalog Image

```
quay.io/mathianasj/freeipa-operator-catalog:latest
```

### Installing from Operator Hub in OpenShift

1. In the OpenShift web console, navigate to **Operators** → **OperatorHub**
2. Search for "FreeIPA" or "freeipa-operator"
3. Select the operator and click **Install**
4. Choose the installation mode (namespace scoped or cluster scoped)
5. Click **Install** to proceed

### Installing via CLI with OLM

```bash
# Create a subscription for the operator
cat <<EOF | oc apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: freeipa-operator
  namespace: operators
spec:
  channel: stable
  name: freeipa-operator
  source: mathianasj-freeipa-operator-catalog
  sourceNamespace: olm
EOF
```

### Creating a CatalogSource (if not auto-discovered)

```bash
cat <<EOF | oc apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: mathianasj-freeipa-operator-catalog
  namespace: olm
spec:
  sourceType: grpc
  image: quay.io/mathianasj/freeipa-operator-catalog:latest
  displayName: FreeIPA Operator
  publisher: mathianasj
EOF
```

## Getting Started

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
# Using the default quay.io/mathianasj registry
make docker-buildx IMG=quay.io/mathianasj/freeipa-operator:latest
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don't work.

### Building and Pushing Bundle/Catalog Images

```sh
# Build and push bundle
make bundle-build BUNDLE_IMG=quay.io/mathianasj/freeipa-operator-bundle:latest
make bundle-push BUNDLE_IMG=quay.io/mathianasj/freeipa-operator-bundle:latest

# Build and push catalog (requires bundle to be pushed first)
make catalog-build \
  CATALOG_IMG=quay.io/mathianasj/freeipa-operator-catalog:latest \
  BUNDLE_IMGS=quay.io/mathianasj/freeipa-operator-bundle:latest
make catalog-push CATALOG_IMG=quay.io/mathianasj/freeipa-operator-catalog:latest
```

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=quay.io/mathianasj/freeipa-operator:latest
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=quay.io/mathianasj/freeipa-operator:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

2. Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/mathianasj/freeipa-operator/<tag or branch>/dist/install.yaml
```

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
