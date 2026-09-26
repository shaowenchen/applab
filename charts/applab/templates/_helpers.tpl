{{/*
Expand the name of the chart.
*/}}
{{- define "applab.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "applab.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version, as used by the chart label.
*/}}
{{- define "applab.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "applab.labels" -}}
helm.sh/chart: {{ include "applab.chart" . }}
{{ include "applab.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: applab
{{- end }}

{{/*
Selector labels. These are immutable on a Deployment, so nothing that can change
between releases may appear here.
*/}}
{{- define "applab.selectorLabels" -}}
app.kubernetes.io/name: {{ include "applab.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The label every object AppLab creates at runtime carries, and the way it is
found again.

applab.io/app holds the app id, so one selector finds everything belonging to one
app: its Deployment, its Service, its VirtualService, its build Jobs. Nothing
AppLab creates is without it, and nothing it did not create has it — which is
what makes a label-based sweep safe, and why the code that deletes an app
verifies the label on every object it is about to remove rather than trusting the
selector (see k8s.Client.checkAppLabels).

The key is spelled out here rather than taken from the Go constant it mirrors,
because a template cannot import one. The two are asserted equal by
hack/helm-check.sh, so a rename in either place fails the build rather than
silently producing a label nothing matches.
*/}}
{{- define "applab.appLabel" -}}
applab.io/app
{{- end }}

{{/*
The namespace AppLab runs in.
*/}}
{{- define "applab.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride }}
{{- end }}

{{/*
The ServiceAccount name.
*/}}
{{- define "applab.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "applab.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The Secret holding the API keys: an existing one if named, otherwise the one this
chart creates.
*/}}
{{- define "applab.secretName" -}}
{{- if .Values.auth.existingSecret }}
{{- .Values.auth.existingSecret }}
{{- else }}
{{- printf "%s-auth" (include "applab.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The Secret holding the object storage credential.

Named separately from the auth Secret because the two are rotated for different
reasons and one may be brought by the operator while the other is built by the
chart.
*/}}
{{- define "applab.objectStoreSecretName" -}}
{{- if .Values.objectStore.existingSecret }}
{{- .Values.objectStore.existingSecret }}
{{- else }}
{{- printf "%s-objectstore" (include "applab.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The path AppLab is served under, or the empty string for the root.

It exists as its own definition because three things have to agree about it and
they are computed in three places:

  APPLAB_BASE_PATH   the server's own prefix, which it matches requests against
                     and strips before routing
  APPLAB_BASE_URL    the address a build clones from, which has to carry it
  ingress.path       what the Ingress routes on, which is where all three come
                     from — an Ingress cannot strip a prefix, so the server has
                     to expect the one the Ingress sends

Only the first used to be derived. The build's address was the Service host and
nothing else, so a build asked for /git/<app>.git at the root of a server that
only answers under /applab, and every clone failed with a 404 the server wrote
itself. Deriving all three from this one definition is what keeps that from
being a thing to remember.

It applies whether or not an Ingress is enabled. It used to be returned only
when one was, on the reasoning that the path was the Ingress's business — but
the server serves the console, the API, git and the apps, and the apps are
nested under this path, so an installation published through the gateway instead
needs the same prefix or every app would be routed somewhere the gateway does not
send requests. The setting is named for the Ingress because that is the case
that needs a path at all; the value is the whole installation's.
*/}}
{{- define "applab.basePath" -}}
{{- if ne (toString .Values.ingress.path) "/" -}}
{{- .Values.ingress.path -}}
{{- end -}}
{{- end }}

{{/*
The address a build clones its source from, and the one reported to clients as
the API's base.

Preferring an explicit public URL and falling back to the in-cluster Service
means a build works either way: it does not depend on the ingress being
reachable from inside the cluster, which it often is not.

The base path is appended here rather than left to the caller, because a URL
without it addresses the root of a deployment that is not at the root. An
explicit apps.baseURL is taken as the whole address and gets nothing appended —
it is the operator saying what the address is, and this is not in a position to
disagree.
*/}}
{{- define "applab.internalURL" -}}
{{- if .Values.apps.baseURL }}
{{- .Values.apps.baseURL }}
{{- else }}
{{- printf "http://%s.%s.svc:%d%s" (include "applab.fullname" .) (include "applab.namespace" .) (int .Values.service.port) (include "applab.basePath" .) }}
{{- end }}
{{- end }}

{{/*
The URL a person reaches AppLab itself at.

Two ways in, and which one applies follows from the same host either way:

  ingress.enabled   the console is reached through the cluster's Ingress, as it
                    always was
  not enabled       there is no Ingress to use, so the console is a VirtualService
                    on the gateway, beside the apps
                    (console-virtualservice.yaml)

An Ingress wins when one is enabled, because enabling it is an explicit
statement about where the console lives; the gateway is the fallback for a
cluster that has no ingress controller at all.

Neither applies when there is no host at all, and then this is empty: the
installation is reachable from inside the cluster only, and a URL invented here
would be one that does not resolve. Callers have to say something else in that
case, which is the point.
*/}}
{{- define "applab.consoleURL" -}}
{{- if not .Values.ingress.host -}}
{{- else if .Values.ingress.enabled -}}
{{- if .Values.ingress.tls -}}
{{- printf "https://%s%s" .Values.ingress.host (include "applab.basePath" .) -}}
{{- else -}}
{{- printf "http://%s%s" .Values.ingress.host (include "applab.basePath" .) -}}
{{- end -}}
{{- else if .Values.deploy.gateway -}}
{{- printf "https://%s%s" .Values.ingress.host (include "applab.basePath" .) -}}
{{- end -}}
{{- end }}

{{/*
Fail early on a configuration that would produce a broken deployment, rather
than letting it fail at runtime where the cause is much harder to see.
*/}}
{{- define "applab.validate" -}}
{{- if and (not .Values.auth.existingSecret) (empty .Values.auth.key) }}
{{- fail "auth.key is empty and auth.existingSecret is not set: set an API key (openssl rand -hex 32), or point auth.existingSecret at a Secret that holds one or more comma-separated" }}
{{- end }}
{{- if .Values.build.enabled }}
{{- if empty .Values.build.registry }}
{{- fail "build.enabled is true but build.registry is empty: builds need a registry to push to. Set build.registry, or set build.enabled=false to run applab without the build pipeline" }}
{{- end }}
{{- end }}
{{/*
A host with no gateway would give every app a hostname that nothing serves:
applab would write a VirtualService whose empty gateway list Istio reads as
mesh-internal only, so the app would deploy, report healthy, and be unreachable
from outside. Refused here rather than discovered from a browser.

The same host is what the console is served on when there is no Ingress, so a
gateway is required for that case too — which is why this is about the host
rather than about the apps.
*/}}
{{- if and (not (empty .Values.ingress.host)) (empty .Values.deploy.gateway) }}
{{- fail "ingress.host is set but deploy.gateway is empty: apps would be given hostnames with no gateway to serve them, and with no Ingress the console would have no route either. Set deploy.gateway to \"<namespace>/<name>\", or leave ingress.host empty to keep everything inside the cluster" }}
{{- end }}
{{- if and (not (empty .Values.deploy.gateway)) (not (contains "/" .Values.deploy.gateway)) }}
{{- fail (printf "deploy.gateway %q must be \"<namespace>/<name>\", the form Istio resolves a gateway by" .Values.deploy.gateway) }}
{{- end }}
{{/*
A path prefix with no host is a deployment where every app is unreachable and no
URL can be reported: the prefix is the only thing telling one app from another,
so there has to be a host for them to share.
*/}}
{{- if and (not (empty .Values.apps.pathPrefix)) (empty .Values.ingress.host) }}
{{- fail "apps.pathPrefix is set but ingress.host is empty: the prefix distinguishes apps on a shared host, so there has to be a host. Set ingress.host, or leave apps.pathPrefix empty to give each app its own subdomain" }}
{{- end }}
{{/*
Apps on a path prefix cannot be published behind an Ingress.

An app is served by its own VirtualService on the gateway, and with a prefix it
is nested under the installation's base path: "/applab/apps/shop". An Ingress
routes on a path and cannot strip one, so the Ingress this chart writes — on
"/applab" — matches that path too and delivers the app's traffic to applab's own
Service, which answers with the console. The app is deployed, healthy and
unreachable, and nothing in the Ingress looks wrong.

There is no arrangement of the Ingress that avoids this: it can only send
sub-paths to one Service, and the console and the apps are different Services.
So the prefix and the Ingress are alternatives, and the gateway serves both —
which is what the console VirtualService is for, and why it is rendered whether
or not an Ingress exists.

Refused here rather than discovered from a browser, and the message names the
way out.
*/}}
{{- if and (not (empty .Values.apps.pathPrefix)) .Values.ingress.enabled }}
{{- fail "apps.pathPrefix is set and ingress.enabled is true: with a prefix every app is nested under ingress.path, and an Ingress routes that whole path to applab itself — the apps would be deployed and unreachable. Set ingress.enabled=false so the gateway serves the console and the apps, or leave apps.pathPrefix empty to give each app its own subdomain" }}
{{- end }}
{{/*
A bucket with no endpoint or no bucket name is the one configuration that fails
silently and expensively.

The server reads an unconfigured object store as "write to ./data/objects", which
is deliberate and useful outside a cluster: it is how the binary runs on a
laptop with nothing else set up. In this chart /data is scratch space — an
emptyDir — so the same fallback does not fail, it *works*, right up until the pod
is replaced. Then every app, every repository and every key is gone, and nothing
in the logs ever said so.

That asymmetry is why this is refused at render time. Off a cluster the fallback
costs a directory; here it costs all of the state, on a schedule nobody controls.
*/}}
{{- if or (empty .Values.objectStore.endpoint) (empty .Values.objectStore.bucket) }}
{{- fail "objectStore.endpoint and objectStore.bucket are both required: without them applab falls back to writing to a directory on the pod's own disk, which in this chart is an emptyDir — it would appear to work and lose every app, every repository and every key on the next restart. objectStore.existingSecret supplies the credential, not the address, so it does not replace these" }}
{{- end }}
{{- end }}
