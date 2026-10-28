# Environment Controller

A Kubernetes controller for managing environments and their relationships across different backends. Currently implements GitHub backend integration that reports deployment status to GitHub's Deployments API and manages environment relationships.

## Features

- **Multi-Backend Support**: Generic Environment API that can support multiple backends (currently GitHub)
- **GitHub Deployment Integration**: Creates and manages GitHub deployments for Environment resources
- **Relationship Management**: Manages environment relationships between environments (After, Alongside)
- **Status Reporting**: Reports deployment status back to GitHub Deployments API
- **Automatic RolloutGate Creation**: Automatically creates and manages RolloutGate resources

## Architecture

The controller manages Environment resources and integrates with backend-specific implementations:

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Environment   │    │  Environment    │    │  GitHub Deploy  │
│   (CRD)          │───▶│  Controller     │───▶│  API            │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                │
                                ▼
                       ┌─────────────────┐
                       │  RolloutGate    │
                       │  (auto-created) │
                       └─────────────────┘
```

## Configuration

The controller uses the Environment CRD for configuration:

```yaml
apiVersion: environments.kuberik.com/v1alpha1
kind: Environment
metadata:
  name: myapp-production
  namespace: default
spec:
  # Reference to the Rollout that this Environment manages
  rolloutRef:
    name: myapp-rollout

  # Environment configuration
  name: "kuberik-myapp-production"
  environment: "production"
  ref: "main"

  # Relationship configuration
  relationship:
    type: "After"
    environment: "staging"

  # Backend-specific configuration
  backend:
    type: "github"
    project: "myorg/myapp"
    secret: "github-token"
```

## Environment Spec

### Required Fields

- `rolloutRef`: Reference to the Rollout resource
- `name`: Name of the deployment (the "kuberik" prefix will be automatically added for GitHub backend)
- `backend`: Backend-specific configuration
  - `type`: Backend type (currently only "github" is supported)
  - `project`: Project identifier (for GitHub: "owner/repo")
  - `secret`: Name of the secret containing backend token (optional, default: "github-token" for GitHub)

### Optional Fields

- `environment`: Environment name (e.g., "production", "staging")
- `ref`: Git reference (branch, tag, or SHA) - defaults to the revision from Rollout history
- `relationship`: Defines relationship to other environments
  - `type`: "After" or "Alongside" (see [Environment Relationships](#environment-relationships))
  - `environment`: Environment name this deployment relates to
- `requeueInterval`: Interval for reconciliation (default: "1m")

## GitHub Token Secret

The controller requires a GitHub token to authenticate with the GitHub API. Create a secret with the token:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-token
  namespace: default
type: Opaque
data:
  token: <base64-encoded-github-token>
```

## Requirements

- The referenced `Rollout` must have deployment history with a `Revision` field in the `VersionInfo` structure
- For GitHub backend, the "kuberik" prefix will be automatically added to deployment names if not already present
- If the revision is not available, the controller will requeue and wait

## How It Works

1. **Environment Detection**: The controller watches for Environment resources with the configured backend.

2. **Backend Validation**: The controller validates that the backend is supported (currently only "github").

3. **Rollout Reference**: The controller fetches the referenced Rollout to get the current deployment version.

4. **Version Resolution**: The controller gets the current version from the Rollout's deployment history, using the `Revision` field from `VersionInfo`.

5. **GitHub Deployment Sync**: For GitHub backend, the controller syncs the entire rollout history with GitHub deployments and statuses.

6. **Status Reporting**: The deployment status is reported back to GitHub's Deployments API.

7. **RolloutGate Management**: The controller automatically creates and manages RolloutGate resources for the Environment.

8. **Relationship Resolution**: If relationships are specified, the controller checks deployment statuses across environments to determine allowed versions.

## Environment Relationships

An Environment with a `relationship` gets a RolloutGate whose `allowedVersions` list comes from the related environment's deployment history, as reported through GitHub deployment statuses. The relationship type decides which of those versions pass:

| Type | A version passes the gate once the related environment's deployment of it is | Use it for |
|------|------------------------------------------------------------------------------|------------|
| `After` | `Succeeded` | Promotion: deploy only what has been deployed and baked successfully before, e.g. `production` after `staging`. |
| `Alongside` | `Deploying`, `InProgress` or `Succeeded` (not `Failed` or `Cancelled`) | Following: deploy the same version as another environment at the same time, e.g. a second production region that should inherit `production`'s manual approvals instead of needing its own. |

```yaml
# prod-eu-1 deploys whatever prod deploys, as soon as prod starts deploying it
spec:
  environment: "prod-eu-1"
  relationship:
    type: "Alongside"
    environment: "prod"
```

The gate is an allow list that the Rollout only consults when it picks its next version. For `Alongside` this means:

- **Failures on the related environment**: when the related environment's deployment of a version fails or is cancelled, the version drops out of the allow list. If this environment has already started deploying it, that deployment keeps going and is judged by this environment's own bake and health checks.
- **Rollbacks don't propagate**: a Rollout only moves to release candidates newer than the version it runs, so when the related environment rolls back, this environment stays where it is.

### Mixed controller versions

Every controller that reports the same deployment `name` to the same GitHub repository reads the other environments' relationships from the GitHub deployment payloads and copies them into its own Environment's status:

- A relationship type the controller doesn't know, for example `Parallel` written by an older controller, is treated as `After`. It is never written to status as-is.
- Older controllers (v0.1.x) don't know `Alongside`, and their CRD rejects it in status. Upgrade the controller and CRD in every cluster that reports to the repository before you set any Environment to `Alongside`. Until then, those older controllers fail to update their status and RolloutGates.

### Upgrading from `Parallel`

`Parallel` has been removed. It never gated differently from `After`: both only passed versions whose deployment on the related environment succeeded. With the new CRD, an Environment that still has `type: Parallel` fails validation on its next update. Until it is updated, the controller treats a stored `Parallel` as `After`.

To upgrade without disruption:

1. Change every `type: Parallel` to `type: After`. Both the old and the new CRD accept it, and it gates exactly as before.
2. Upgrade the controller and CRD in every cluster that reports to the same GitHub repository.
3. Set `type: Alongside` on the environments that should follow another one.

## Installation

### Prerequisites

- Kubernetes cluster
- kubectl configured
- GitHub token with appropriate permissions (for GitHub backend)

### Install the Controller

1. **Install CRDs**:
   ```bash
   kubectl apply -f config/crd/bases/
   ```

2. **Install the controller**:
   ```bash
   kubectl apply -k config/default/
   ```

3. **Create GitHub token secret**:
   ```bash
   kubectl apply -f config/samples/github-token-secret.yaml
   ```

4. **Create Environment resources**:
   ```bash
   kubectl apply -k config/samples/
   ```

## Development

### Building

```bash
make build
```

### Testing

```bash
make test
```

### Running Locally

```bash
make run
```

## API Reference

### Environment Spec

```go
type EnvironmentSpec struct {
    RolloutRef       corev1.LocalObjectReference `json:"rolloutRef"`
    Name             string                      `json:"name"`
    Environment      string                      `json:"environment,omitempty"`
    Ref              string                      `json:"ref,omitempty"`
    Relationship     *EnvironmentRelationship    `json:"relationship,omitempty"`
    Backend          BackendConfig               `json:"backend"`
    RequeueInterval  string                      `json:"requeueInterval,omitempty"`
}

type BackendConfig struct {
    Type             string   `json:"type"`
    Project          string   `json:"project"`
    Secret           string   `json:"secret,omitempty"`
}
```

### EnvironmentRelationship

```go
type EnvironmentRelationship struct {
    Environment string           `json:"environment"`
    Type        RelationshipType `json:"type"` // "After" or "Alongside"
}
```

### Environment Status

```go
type EnvironmentStatus struct {
    DeploymentID      *int64                    `json:"deploymentId,omitempty"`
    DeploymentURL     string                    `json:"deploymentUrl,omitempty"`
    DeploymentStatuses []EnvironmentStatusEntry  `json:"deploymentStatuses,omitempty"`
    EnvironmentInfos  []EnvironmentInfo         `json:"environmentInfos,omitempty"`
}
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Submit a pull request

## License

Apache 2.0
