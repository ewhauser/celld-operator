{{- define "celld-operator.name" -}}
{{- printf "%s-celld-operator" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "celld-operator.labels" -}}
app.kubernetes.io/name: celld-operator
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}
{{- define "celld-operator.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- end -}}
