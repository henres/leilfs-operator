# leilfs-operator
Open-source Kubernetes operator for deploying and managing LeilFS clusters.

## Description
This operator introduces the LeilFSCluster CRD to configure master and chunk
components and optionally deploy a CSI driver. It focuses on cluster
configuration, scheduling, and resource controls to run LeilFS on Kubernetes.

## Getting Started

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/leilfs-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified. 
And it is required to have access to pull the image from the working environment. 
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/leilfs-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin 
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Update the sample fields to match your images and storage settings.

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

`dist/install.yaml` is a generated build artefact (see `.gitignore`) — it
is never committed to the repo tree. It is produced and published in two
ways:

- **Tagged releases** (`v0.1.0`, `v1.2.3`, ...): `.github/workflows/release.yml`
  runs `make build-installer` with the just-published image reference and
  attaches the result to the corresponding [GitHub Release](https://github.com/henres/leilfs-operator/releases)
  as `install.yaml`. See "Cutting a release" in `AGENTS.md` for how a
  release is cut and what else the workflow does (CHANGELOG.md, Helm
  chart).
- **Local/manual builds**: build it yourself for any image reference:

```sh
make build-installer IMG=<some-registry>/leilfs-operator:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

### Using the installer

Once a release exists, install directly from its Release asset (this is
the URL that actually resolves — a plain `raw.githubusercontent.com` path
would 404, since `dist/install.yaml` is intentionally not committed):

```sh
kubectl apply -f https://github.com/henres/leilfs-operator/releases/download/<tag>/install.yaml
```

Or install via the Helm chart published alongside every release:

```sh
helm install leilfs-operator oci://ghcr.io/henres/leilfs-operator/charts/leilfs-operator --version <version>
```

## Contributing
Contributions are welcome. Please open an issue to discuss changes before
submitting a pull request.

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

