{{/*
Chart name truncated to 63 chars (DNS label limit).
*/}}
{{- define "scale-sentry.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. If release name already contains the chart name,
collapse it; otherwise prefix the release name to the chart name.
*/}}
{{- define "scale-sentry.fullname" -}}
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

{{- define "scale-sentry.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "scale-sentry.labels" -}}
helm.sh/chart: {{ include "scale-sentry.chart" . }}
{{ include "scale-sentry.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "scale-sentry.selectorLabels" -}}
app.kubernetes.io/name: {{ include "scale-sentry.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "scale-sentry.controllerServiceAccountName" -}}
{{- printf "%s-controller" (include "scale-sentry.fullname" .) -}}
{{- end -}}
