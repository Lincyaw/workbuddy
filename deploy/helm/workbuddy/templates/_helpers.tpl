{{/*
Expand the name of the chart.
*/}}
{{- define "workbuddy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "workbuddy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name + version label.
*/}}
{{- define "workbuddy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "workbuddy.labels" -}}
helm.sh/chart: {{ include "workbuddy.chart" . }}
{{ include "workbuddy.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "workbuddy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "workbuddy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Image reference (repository:tag), defaulting tag to AppVersion.
*/}}
{{- define "workbuddy.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Name of the Gitea-token Secret to mount. If user supplied an existing one,
use it as-is; otherwise refer to the chart-managed Secret.
*/}}
{{- define "workbuddy.giteaSecretName" -}}
{{- if .Values.giteaToken.secretName -}}
{{- .Values.giteaToken.secretName -}}
{{- else -}}
{{- printf "%s-gitea" (include "workbuddy.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the agent-configs ConfigMap to mount. Same pattern as Gitea Secret.
*/}}
{{- define "workbuddy.agentsConfigMapName" -}}
{{- if .Values.agents.configMapName -}}
{{- .Values.agents.configMapName -}}
{{- else -}}
{{- printf "%s-agents" (include "workbuddy.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Name of the coordinator-config ConfigMap (config.yaml + workflows/default.md).
*/}}
{{- define "workbuddy.configConfigMapName" -}}
{{- printf "%s-config" (include "workbuddy.fullname" .) -}}
{{- end -}}

{{/*
Name of the auth-token Secret. Existing one if supplied, else chart-managed.
*/}}
{{- define "workbuddy.authSecretName" -}}
{{- if .Values.auth.secretName -}}
{{- .Values.auth.secretName -}}
{{- else -}}
{{- printf "%s-auth" (include "workbuddy.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
In-container config directory passed to `serve --config-dir`.
*/}}
{{- define "workbuddy.configDir" -}}/etc/workbuddy{{- end -}}

{{/*
Resolved report base URL: explicit value, else the in-cluster Service DNS.
*/}}
{{- define "workbuddy.reportBaseURL" -}}
{{- if .Values.reportBaseURL -}}
{{- .Values.reportBaseURL -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%v" (include "workbuddy.fullname" .) .Release.Namespace .Values.service.port -}}
{{- end -}}
{{- end -}}
