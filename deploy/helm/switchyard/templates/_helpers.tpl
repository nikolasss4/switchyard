{{- define "switchyard.name" -}}{{ .Release.Name }}-switchyard{{- end -}}
{{- define "switchyard.labels" -}}
app.kubernetes.io/name: switchyard
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}
{{- define "switchyard.selector" -}}
app.kubernetes.io/name: switchyard
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
