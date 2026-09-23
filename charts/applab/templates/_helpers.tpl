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
{{- if and .Values.ingress.enabled (empty .Values.apps.baseDomain) }}
{{- fail "ingress.enabled is true but apps.baseDomain is empty: apps need a domain to be served under. Set apps.baseDomain, or set ingress.enabled=false" }}
{{- end }}
{{- if and .Values.persistence.enabled (not .Values.persistence.existingClaim) (empty .Values.persistence.storageClass) (not (hasKey .Values.persistence "storageClass")) }}
{{- end }}
{{- end }}
