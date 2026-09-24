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
The namespace applab runs in.
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
The address a build job's init container uses to fetch its source.

Preferring an explicit public URL and falling back to the in-cluster Service
means a build works either way: it does not depend on the ingress being
reachable from inside the cluster, which it often is not.
*/}}
{{- define "applab.internalURL" -}}
{{- if .Values.apps.baseURL }}
{{- .Values.apps.baseURL }}
{{- else }}
{{- printf "http://%s.%s.svc:%d" (include "applab.fullname" .) (include "applab.namespace" .) (int .Values.service.port) }}
{{- end }}
{{- end }}

{{/*
The URL a person reaches applab itself at.

Two ways in, and which one applies follows from the same settings that decide
how apps are published:

  ingress.enabled          the console is reached through the cluster's Ingress,
                           as it always was
  apps.baseDomain + gateway  there is no Ingress to use, so the console is a
                           VirtualService on the gateway, beside the apps
                           (console-virtualservice.yaml)

An Ingress wins when one is enabled, because enabling it is an explicit
statement about where the console lives; the gateway is the fallback for a
cluster that has no ingress controller at all.

Neither applies when there is no Ingress and no base domain, and then this is
empty: the installation is reachable from inside the cluster only, and a URL
invented here would be one that does not resolve. Callers have to say something
else in that case, which is the point.
*/}}
{{- define "applab.consoleURL" -}}
{{- if and .Values.ingress.enabled .Values.ingress.hosts -}}
{{- if .Values.ingress.tls -}}
{{- printf "https://%s" (index .Values.ingress.hosts 0).host -}}
{{- else -}}
{{- printf "http://%s" (index .Values.ingress.hosts 0).host -}}
{{- end -}}
{{- else if and .Values.deploy.gateway .Values.apps.baseDomain -}}
{{- printf "https://%s" .Values.apps.baseDomain -}}
{{- end -}}
{{- end }}

{{/*
Fail early on a configuration that would produce a broken deployment, rather
than letting it fail at runtime where the cause is much harder to see.
*/}}
{{- define "applab.validate" -}}
{{- if and (not .Values.auth.existingSecret) (empty .Values.auth.keys) }}
{{- fail "auth.keys is empty and auth.existingSecret is not set: set at least one API key (openssl rand -hex 32), or point auth.existingSecret at a Secret that holds one" }}
{{- end }}
{{- if .Values.build.enabled }}
{{- if empty .Values.build.registry }}
{{- fail "build.enabled is true but build.registry is empty: builds need a registry to push to. Set build.registry, or set build.enabled=false to run applab without the build pipeline" }}
{{- end }}
{{- end }}
{{/*
A base domain with no gateway would give every app a hostname that nothing
serves: applab would write a VirtualService whose empty gateway list Istio reads
as mesh-internal only, so the app would deploy, report healthy, and be
unreachable from outside. Refused here rather than discovered from a browser.
*/}}
{{- if and (not (empty .Values.apps.baseDomain)) (empty .Values.deploy.gateway) }}
{{- fail "apps.baseDomain is set but deploy.gateway is empty: apps would be given hostnames with no gateway to serve them, so they would be unreachable from outside the cluster. Set deploy.gateway to \"<namespace>/<name>\", or leave apps.baseDomain empty to serve apps inside the cluster only" }}
{{- end }}
{{- if and (not (empty .Values.deploy.gateway)) (not (contains "/" .Values.deploy.gateway)) }}
{{- fail (printf "deploy.gateway %q must be \"<namespace>/<name>\", the form Istio resolves a gateway by" .Values.deploy.gateway) }}
{{- end }}
{{/*
A path prefix with no domain is a deployment where every app is unreachable and
no URL can be reported: the prefix is the only thing telling one app from
another, so there has to be a host for them to share.
*/}}
{{- if and (not (empty .Values.apps.pathPrefix)) (empty .Values.apps.baseDomain) }}
{{- fail "apps.pathPrefix is set but apps.baseDomain is empty: the prefix distinguishes apps on a shared host, so there has to be a host. Set apps.baseDomain, or leave apps.pathPrefix empty to give each app its own subdomain" }}
{{- end }}
{{- end }}
