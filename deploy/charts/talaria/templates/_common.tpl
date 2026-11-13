{{- /*
talaria.common: Talaria's short values mapped onto the bjw-s common library's values (controllers, service,
persistence, configMaps, secrets, ingress, route). common.yaml renders the library with them.

Every object has a fixed name (forceRename), so the volumes, the Secrets and the Service addresses stay the same
across upgrades.
*/ -}}
{{- define "talaria.common" -}}
{{- $full := include "talaria.fullname" . -}}
{{- $v := .Values -}}
{{- $pull := $v.images.pullPolicy -}}
{{- $rolling := $v.hermes.rolling.enabled -}}
{{- $mode := include "talaria.binaryMode" . -}}
{{- $bridge := $v.hermes.editorBridge.enabled -}}
{{- $sqlite := eq $v.hub.store.driver "sqlite" -}}
{{- $searxng := $v.searxng.enabled -}}
{{- if and (gt (int $v.hub.replicas) 1) $sqlite }}
{{- fail "hub.replicas above 1 needs hub.store.driver postgres" }}
{{- end }}
{{- $secrets := include "talaria.secrets" . | fromYaml -}}
{{- $files := dict
  "hermes-init.py" (.Files.Get "files/hermes-init.py")
  "demo-llm.py" (.Files.Get "files/demo-llm.py")
-}}
{{- if $searxng }}
{{- $_ := set $files "searxng-settings.yml" (.Files.Get "files/searxng-settings.yml") -}}
{{- end }}
{{- $sc := dict "readOnlyRootFilesystem" true "allowPrivilegeEscalation" false "capabilities" (dict "drop" (list "ALL")) -}}
global:
  # Values are not evaluated as templates: the agent's config and the setup scripts may hold {{ "{{" }} as text
  templateEvaluation:
    passes: 0
  createDefaultServiceAccount: false
  propagateGlobalMetadataToPods: false

defaultPodOptions:
  automountServiceAccountToken: false
  enableServiceLinks: false
  {{- with $v.imagePullSecrets }}
  imagePullSecrets: {{- toYaml . | nindent 4 }}
  {{- end }}

controllers:
  hub:
    type: deployment
    forceRename: {{ $full }}-hub
    extraSelectorLabels:
      app.kubernetes.io/component: hub
    replicas: {{ $v.hub.replicas }}
    {{- if $sqlite }}
    # One SQLite file on a ReadWriteOnce volume: the old pod stops before the new one starts
    strategy: Recreate
    {{- else }}
    # Several replicas share PostgreSQL; a stopping replica fails /readyz first and drains (shutdown.drain_delay)
    strategy: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    {{- end }}
    {{- if gt (int $v.hub.replicas) 1 }}
    podDisruptionBudget:
      minAvailable: 1
    {{- end }}
    pod:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        fsGroup: 65534
        fsGroupChangePolicy: OnRootMismatch
        seccompProfile: {type: RuntimeDefault}
    containers:
      # The pod's one port. The hub flushes each chat event as it writes it, so no proxy sits in front of it here
      hub:
        image: {{- include "talaria.image" (list $v.images.hub $pull) | nindent 10 }}
        env:
          - {name: CONFIG, value: /config/hub.yaml}
          - {name: TMPDIR, value: /tmp}
          - {name: HUB_HTTP_PORT, value: "8081"}
          - {name: HUB_HTTP_PUBLIC_URL, value: {{ include "talaria.publicUrl" . | quote }}}
          - {name: HUB_DEVICE_ENABLED, value: "true"}
          # Ingress and gateway proxies in front of the hub
          - {name: HUB_DEVICE_TRUSTED_PROXIES, value: "127.0.0.1/32,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"}
          - {name: HUB_UI_TIME_ZONE, value: {{ $v.timeZone | quote }}}
          - {name: HUB_UI_INSTALL, value: helm}
          - {name: HUB_CLAUDE_TIME_ZONE, value: {{ $v.timeZone | quote }}}
          - {name: HUB_STORE_DRIVER, value: {{ $v.hub.store.driver | quote }}}
          {{- if $sqlite }}
          - {name: HUB_STORE_PATH, value: /data/talaria-hub.db}
          {{- else }}
          - name: HUB_STORE_DSN
            valueFrom:
              secretKeyRef: {name: {{ required "hub.store.existingSecret is required with driver postgres" $v.hub.store.existingSecret }}, key: dsn}
          {{- end }}
          {{- if and (eq $v.auth.mode "oidc") $v.auth.oidc.existingSecret }}
          - name: HUB_OIDC_CLIENT_SECRET
            valueFrom:
              secretKeyRef: {name: {{ $v.auth.oidc.existingSecret }}, key: client-secret}
          {{- end }}
          {{- if $v.telegram.existingSecret }}
          - {name: HUB_TELEGRAM_ENABLED, value: "true"}
          - name: HUB_TELEGRAM_TOKEN
            valueFrom:
              secretKeyRef: {name: {{ $v.telegram.existingSecret }}, key: token}
          {{- end }}
          {{- if $v.hub.caBundle }}
          # The system's authorities and the ones in hub.caBundle
          - {name: SSL_CERT_DIR, value: /etc/ssl/certs:/etc/talaria-ca}
          {{- end }}
          {{- range $k, $val := $v.hub.env }}
          - {name: {{ $k }}, value: {{ $val | quote }}}
          {{- end }}
        ports:
          - {name: http, containerPort: 8081, protocol: TCP}
        probes:
          # Fails as soon as a stopping replica starts to drain
          readiness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /readyz, port: 8081}
              periodSeconds: 2
              timeoutSeconds: 2
              failureThreshold: 1
          liveness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /healthz, port: 8081}
              periodSeconds: 30
              timeoutSeconds: 5
          startup:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /healthz, port: 8081}
              periodSeconds: 2
              failureThreshold: 30
        resources: {{- toYaml $v.hub.resources | nindent 10 }}
        securityContext: {{- toYaml $sc | nindent 10 }}

  hermes:
    type: deployment
    forceRename: {{ $full }}-hermes
    extraSelectorLabels:
      app.kubernetes.io/component: hermes
    replicas: 1
    {{- if $rolling }}
    # The new pod boots next to the old one as a standby: its gateway waits for the old gateway's lock and takes
    # over within a second of its exit. maxUnavailable 0 would deadlock: the old pod only stops once the new one
    # is ready, and the new one is ready only after the old one let go
    strategy: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 1
    {{- else }}
    strategy: Recreate
    {{- end }}
    pod:
      {{- if $rolling }}
      terminationGracePeriodSeconds: {{ $v.hermes.rolling.terminationGracePeriodSeconds }}
      {{- if eq $v.hermes.persistence.accessMode "ReadWriteOnce" }}
      # Both pods mount the ReadWriteOnce volume during the handover, so the new pod goes to the old pod's node
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app.kubernetes.io/name: {{ .Chart.Name }}
                  app.kubernetes.io/instance: {{ .Release.Name }}
                  app.kubernetes.io/component: hermes
              topologyKey: kubernetes.io/hostname
      {{- end }}
      {{- else }}
      terminationGracePeriodSeconds: 120
      {{- end }}
      securityContext:
        # The image's hermes user
        runAsNonRoot: true
        runAsUser: 10000
        runAsGroup: 10000
        fsGroup: 10000
        fsGroupChangePolicy: OnRootMismatch
        seccompProfile: {type: RuntimeDefault}
    {{- $env := list
      (dict "name" "HOME" "value" "/opt/data")
      (dict "name" "HERMES_HOME" "value" "/opt/data")
      (dict "name" "HERMES_WRITE_SAFE_ROOT" "value" "/opt/data:/work")
      (dict "name" "TZ" "value" $v.timeZone)
      (dict "name" "TALARIA_SECRETS_DIR" "value" "/secrets")
      (dict "name" "TALARIA_USER" "value" $v.auth.user)
      (dict "name" "TALARIA_SKILLS_SYNC" "value" "1")
      (dict "name" "TALARIA_EXTRA_CONFIG" "value" "/secrets/hermes-extra.yaml")
    }}
    {{- if $rolling }}{{ $env = append $env (dict "name" "TALARIA_ROLLING" "value" "1") }}{{ end }}
    {{- if $v.demo.enabled }}
    {{- /* The demo model, offered as the Hermes provider talaria-demo */ -}}
    {{- $env = append $env (dict "name" "TALARIA_DEMO_URL" "value" (printf "http://%s-demo-llm:8000/v1" $full)) }}
    {{- end }}
    {{- if $searxng }}
    {{- /* Web search through the chart's SearXNG: hermes-init.py makes it the search backend while none is chosen */ -}}
    {{- $env = append $env (dict "name" "TALARIA_SEARXNG_URL" "value" (printf "http://%s-searxng:8080" $full)) }}
    {{- end }}
    {{- range $k, $val := $v.hermes.env }}{{ $env = append $env (dict "name" $k "value" (toString $val)) }}{{ end }}
    initContainers:
      # The device plugin from the talaria image, so client, plugin and protocol share one release
      plugin:
        image: {{- include "talaria.image" (list $v.images.device $pull) | nindent 10 }}
        args: ["plugin", "install", "--dest", "/plugin-src/device"]
        resources:
          requests: {cpu: 10m, memory: 16Mi}
          limits: {memory: 64Mi}
        securityContext: {{- toYaml $sc | nindent 10 }}
      {{- if and $bridge (eq $mode "copy") }}
      # Clusters older than 1.35 (image volumes need a feature gate): the talaria binary for acp-host, copied into a
      # shared emptyDir (the image has no shell or cp)
      device-bin:
        dependsOn: plugin
        image: {{- include "talaria.image" (list $v.images.device $pull) | nindent 10 }}
        args: ["copy-self", "/device-bin/talaria"]
        resources:
          requests: {cpu: 10m, memory: 16Mi}
          limits: {memory: 128Mi}
        securityContext: {{- toYaml $sc | nindent 10 }}
      {{- end }}
      # Device plugin, hub public key, Talaria settings in config.yaml
      init:
        dependsOn: {{ if and $bridge (eq $mode "copy") }}device-bin{{ else }}plugin{{ end }}
        image: {{- include "talaria.image" (list $v.images.hermes $pull) | nindent 10 }}
        command: ["/opt/hermes/.venv/bin/python", "-I", "/talaria/hermes-init.py"]
        env: {{- toYaml $env | nindent 10 }}
        resources:
          requests: {cpu: 10m, memory: 128Mi}
          limits: {memory: 1Gi}
        securityContext: {{- toYaml $sc | nindent 10 }}
    containers:
      gateway:
        image: {{- include "talaria.image" (list $v.images.hermes $pull) | nindent 10 }}
        command: ["/opt/hermes/.venv/bin/hermes", "gateway", "run", "--no-supervise", "--external-supervisor"]
        workingDir: /opt/data
        env: {{- toYaml $env | nindent 10 }}
        ports:
          - {name: webapp, containerPort: 9120, protocol: TCP}
        probes:
          {{- if $rolling }}
          # A warmed-up standby (gateway.standby on the volume) counts as started, so it can wait for the old pod's
          # gateway; only readiness gates the takeover
          startup:
            enabled: true
            custom: true
            spec:
              exec:
                command: ["/bin/sh", "-c", "test -e /opt/data/gateway.standby || curl -fsS -m 5 -o /dev/null http://127.0.0.1:9120/api/health"]
              periodSeconds: 1
              timeoutSeconds: 6
              failureThreshold: 960
          {{- else }}
          startup:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /api/health, port: 9120}
              periodSeconds: 2
              timeoutSeconds: 5
              failureThreshold: 150
          {{- end }}
          liveness:
            enabled: true
            custom: true
            spec:
              exec:
                command: ["/bin/sh", "-c", "test -e /opt/data/gateway.standby || curl -fsS -m 10 -o /dev/null http://127.0.0.1:9120/api/health"]
              periodSeconds: 30
              timeoutSeconds: 11
              failureThreshold: 5
          # 200 while serving and in the restart wait (approvals stay answerable), 503 while starting or stopping
          readiness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /api/health, port: 9120}
              periodSeconds: 1
              timeoutSeconds: 5
              failureThreshold: 2
        {{- if $rolling }}
        lifecycle:
          preStop:
            exec:
              # Serve on until the new pod's gateway has warmed up and waits for the lock, then let SIGTERM open the
              # restart wait. A stop with no successor goes on after 2 minutes
              command: ["/bin/sh", "-c", "s=$(date +%s); while [ $(($(date +%s)-s)) -lt 120 ]; do m=$(stat -c %Y /opt/data/gateway.standby 2>/dev/null || echo 0); [ $m -ge $((s-1)) ] && exit 0; sleep 0.5; done"]
        {{- end }}
        resources: {{- toYaml $v.hermes.resources | nindent 10 }}
        securityContext: {{- toYaml $sc | nindent 10 }}
      {{- if $bridge }}
      # Hosts one hermes acp per editor connection that the hub forwards (talaria acp). Its own container, so editor
      # sessions outlive gateway restarts; the binary comes from the talaria image (copied by an init container, or
      # mounted read-only as an image volume)
      acp-host:
        image: {{- include "talaria.image" (list $v.images.hermes $pull) | nindent 10 }}
        command: [{{ printf "/opt/talaria/%s" (ternary "talaria" $v.images.deviceBinary (eq $mode "copy")) | quote }}, "acp-host"]
        workingDir: /opt/data
        env:
          - {name: HOME, value: /opt/data}
          - {name: HERMES_HOME, value: /opt/data}
          - {name: TMPDIR, value: /tmp}
          - {name: TZ, value: {{ $v.timeZone | quote }}}
          - {name: TALARIA_UPDATE_AUTO, value: "false"}
          - {name: TALARIA_ACP_HOST_LISTEN, value: ":9130"}
          - {name: TALARIA_ACP_HOST_BACKEND_ID, value: main}
          - {name: TALARIA_ACP_HOST_PLACEHOLDER, value: /opt/data/device-placeholder}
          - {name: TALARIA_ACP_HOST_HERMES, value: /opt/hermes/.venv/bin/hermes}
          - {name: TALARIA_ACP_HOST_HUB_PUBLIC_KEY_FILE, value: /opt/data/talaria/hub_public_key.pem}
        ports:
          - {name: acp, containerPort: 9130, protocol: TCP}
        probes:
          liveness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /healthz, port: 9130}
              periodSeconds: 30
        resources:
          requests: {cpu: 10m, memory: 64Mi}
          limits: {memory: 4Gi}
        securityContext: {{- toYaml $sc | nindent 10 }}
      {{- end }}

  # Where the agent runs commands for chats that are not on one of your computers. One connection per sandbox
  # device: a second pod would replace the first one's, so it is replaced, not rolled
  sandbox:
    type: deployment
    forceRename: {{ $full }}-sandbox
    extraSelectorLabels:
      app.kubernetes.io/component: sandbox
    replicas: 1
    strategy: Recreate
    pod:
      securityContext:
        runAsNonRoot: true
        runAsUser: 568
        runAsGroup: 568
        fsGroup: 568
        fsGroupChangePolicy: OnRootMismatch
        seccompProfile: {type: RuntimeDefault}
    containers:
      sandbox:
        image: {{- include "talaria.image" (list $v.images.sandbox $pull) | nindent 10 }}
        env:
          - {name: TALARIA_HUB_URL, value: "http://{{ $full }}-hub-internal:8081"}
          - {name: TALARIA_HUB_TOKEN_FILE, value: /secrets/token}
        resources: {{- toYaml $v.sandbox.resources | nindent 10 }}
        securityContext: {{- toYaml $sc | nindent 10 }}

  # A stand-in model with no API key, to try Talaria. It answers with a canned reply
  demo-llm:
    enabled: {{ $v.demo.enabled }}
    type: deployment
    forceRename: {{ $full }}-demo-llm
    extraSelectorLabels:
      app.kubernetes.io/component: demo-llm
    replicas: 1
    strategy: RollingUpdate
    pod:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
        runAsGroup: 65534
        seccompProfile: {type: RuntimeDefault}
    containers:
      demo-llm:
        image: {{- include "talaria.image" (list $v.images.hermes $pull) | nindent 10 }}
        command: ["/opt/hermes/.venv/bin/python", "-I", "/talaria/demo-llm.py"]
        ports:
          - {name: http, containerPort: 8000, protocol: TCP}
        probes:
          readiness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /health, port: 8000}
        resources:
          requests: {cpu: 5m, memory: 32Mi}
          limits: {memory: 128Mi}
        securityContext: {{- toYaml $sc | nindent 10 }}

  {{- if $searxng }}
  # SearXNG, the metasearch engine behind the agent's web_search: no API key, and only the agent reaches it
  searxng:
    type: deployment
    forceRename: {{ $full }}-searxng
    extraSelectorLabels:
      app.kubernetes.io/component: searxng
    replicas: 1
    pod:
      securityContext:
        # The image's searxng user
        runAsNonRoot: true
        runAsUser: 977
        runAsGroup: 977
        fsGroup: 977
        seccompProfile: {type: RuntimeDefault}
    containers:
      searxng:
        image: {{- include "talaria.image" (list $v.images.searxng $pull) | nindent 10 }}
        env:
          - {name: SEARXNG_SETTINGS_PATH, value: /etc/searxng/settings.yml}
          - {name: SEARXNG_PORT, value: "8080"}
          - {name: GRANIAN_HOST, value: "0.0.0.0"}
          - {name: SEARXNG_BASE_URL, value: "http://{{ $full }}-searxng:8080/"}
          - {name: GRANIAN_WORKERS, value: "1"}
          - name: SEARXNG_SECRET
            valueFrom:
              secretKeyRef: {name: {{ $full }}-config, key: searxng-secret}
        ports:
          - {name: http, containerPort: 8080, protocol: TCP}
        probes:
          readiness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /healthz, port: 8080}
              periodSeconds: 10
          liveness:
            enabled: true
            custom: true
            spec:
              httpGet: {path: /healthz, port: 8080}
              periodSeconds: 30
              timeoutSeconds: 5
        resources: {{- toYaml $v.searxng.resources | nindent 10 }}
        securityContext: {{- toYaml $sc | nindent 10 }}
  {{- end }}

service:
  hub:
    controller: hub
    forceRename: {{ $full }}
    type: {{ $v.service.type }}
    ports:
      http:
        port: {{ $v.service.port }}
        targetPort: http
  # The address the sandbox dials
  hub-internal:
    controller: hub
    forceRename: {{ $full }}-hub-internal
    ports:
      http:
        port: 8081
        targetPort: 8081
  hermes:
    controller: hermes
    forceRename: {{ $full }}-hermes
    ports:
      webapp:
        port: 9120
        targetPort: webapp
      acp:
        enabled: {{ $bridge }}
        port: 9130
        targetPort: acp
  demo-llm:
    enabled: {{ $v.demo.enabled }}
    controller: demo-llm
    forceRename: {{ $full }}-demo-llm
    ports:
      http:
        port: 8000
        targetPort: http
  searxng:
    enabled: {{ $searxng }}
    controller: searxng
    forceRename: {{ $full }}-searxng
    ports:
      http:
        port: 8080
        targetPort: http

configMaps:
  files:
    forceRename: {{ $full }}-files
    data: {{- toYaml $files | nindent 6 }}
  ca:
    enabled: {{ not (empty $v.hub.caBundle) }}
    forceRename: {{ $full }}-ca
    data:
      ca.crt: {{ $v.hub.caBundle | quote }}

secrets: {{- toYaml $secrets | nindent 2 }}

persistence:
  {{- if $sqlite }}
  hub-data:
    type: persistentVolumeClaim
    forceRename: {{ $full }}-hub
    retain: true
    accessMode: ReadWriteOnce
    size: {{ $v.hub.persistence.size }}
    {{- with $v.hub.persistence.storageClass }}
    storageClass: {{ . }}
    {{- end }}
    advancedMounts:
      hub:
        hub:
          - path: /data
  {{- end }}
  hermes-data:
    type: persistentVolumeClaim
    forceRename: {{ $full }}-hermes
    retain: true
    accessMode: {{ $v.hermes.persistence.accessMode }}
    size: {{ $v.hermes.persistence.size }}
    {{- with $v.hermes.persistence.storageClass }}
    storageClass: {{ . }}
    {{- end }}
    advancedMounts:
      hermes:
        init: [{path: /opt/data}]
        gateway: [{path: /opt/data}]
        {{- if $bridge }}
        acp-host: [{path: /opt/data}]
        {{- end }}
  sandbox-work:
    type: persistentVolumeClaim
    forceRename: {{ $full }}-sandbox
    retain: true
    accessMode: ReadWriteOnce
    size: {{ $v.sandbox.persistence.size }}
    {{- with $v.sandbox.persistence.storageClass }}
    storageClass: {{ . }}
    {{- end }}
    advancedMounts:
      sandbox:
        sandbox: [{path: /work}]
  tmp:
    type: emptyDir
    advancedMounts:
      hub:
        hub: [{path: /tmp}]
      hermes:
        init: [{path: /tmp}]
        gateway: [{path: /tmp}]
        {{- if $bridge }}
        acp-host: [{path: /tmp}]
        {{- end }}
      sandbox:
        sandbox: [{path: /tmp}]
      {{- if $searxng }}
      searxng:
        searxng: [{path: /tmp}, {path: /etc/searxng, subPath: searxng-etc}, {path: /var/cache/searxng, subPath: searxng-cache}]
      {{- end }}
  files:
    type: configMap
    identifier: files
    advancedMounts:
      hermes:
        init: [{path: /talaria, readOnly: true}]
        gateway: [{path: /talaria, readOnly: true}]
      {{- if $v.demo.enabled }}
      demo-llm:
        demo-llm: [{path: /talaria, readOnly: true}]
      {{- end }}
      {{- if $searxng }}
      searxng:
        searxng: [{path: /etc/searxng/settings.yml, subPath: searxng-settings.yml, readOnly: true}]
      {{- end }}
  hub-config:
    type: secret
    name: {{ $full }}-config
    defaultMode: 0440
    advancedMounts:
      hub:
        hub: [{path: /config/hub.yaml, subPath: hub.yaml, readOnly: true}]
  {{- if $v.hub.caBundle }}
  ca:
    type: configMap
    identifier: ca
    advancedMounts:
      hub:
        hub: [{path: /etc/talaria-ca, readOnly: true}]
  {{- end }}
  plugin-src:
    type: emptyDir
    advancedMounts:
      hermes:
        plugin: [{path: /plugin-src}]
        init: [{path: /plugin-src, readOnly: true}]
        gateway: [{path: /plugin-src, readOnly: true}]
  secrets:
    type: secret
    name: {{ $full }}-config
    items:
      - {key: hub-signing-key, path: hub-signing-key}
      - {key: password, path: password}
      - {key: session-secret, path: session-secret}
      - {key: hermes-extra.yaml, path: hermes-extra.yaml}
    advancedMounts:
      hermes:
        init: [{path: /secrets, readOnly: true}]
        gateway: [{path: /secrets, readOnly: true}]
  sandbox-token:
    type: secret
    name: {{ $full }}-config
    defaultMode: 0440
    items:
      - {key: sandbox-token, path: token}
    advancedMounts:
      sandbox:
        sandbox: [{path: /secrets, readOnly: true}]
  {{- if $bridge }}
  device-bin:
    {{- if eq $mode "copy" }}
    type: emptyDir
    {{- else }}
    # An image volume; written as a custom volume so that a forced "image" renders on any detected version
    type: custom
    volumeSpec:
      image:
        reference: {{ $v.images.device }}
        pullPolicy: {{ $pull }}
    {{- end }}
    advancedMounts:
      hermes:
        {{- if eq $mode "copy" }}
        device-bin: [{path: /device-bin}]
        {{- end }}
        acp-host: [{path: /opt/talaria, readOnly: true}]
  {{- end }}

ingress:
  {{- if $v.ingress.enabled }}
  hub:
    forceRename: {{ $full }}
    {{- with $v.ingress.className }}
    className: {{ . }}
    {{- end }}
    annotations:
      # Chats stream as server-sent events and devices hold WebSockets: no buffering, long timeouts
      nginx.ingress.kubernetes.io/proxy-buffering: "off"
      nginx.ingress.kubernetes.io/proxy-read-timeout: "86400"
      nginx.ingress.kubernetes.io/proxy-send-timeout: "86400"
      nginx.ingress.kubernetes.io/proxy-body-size: 64m
      {{- with $v.ingress.annotations }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    hosts:
      - host: {{ include "talaria.host" . }}
        paths:
          - path: /
            pathType: Prefix
            service:
              identifier: hub
              port: http
    {{- with $v.ingress.tlsSecret }}
    tls:
      - hosts: [{{ include "talaria.host" $ }}]
        secretName: {{ . }}
    {{- end }}
  {{- end }}

route:
  {{- if $v.httpRoute.enabled }}
  # Two routes: an authenticating policy (such as an Envoy Gateway SecurityPolicy) can target the first one, for
  # browsers, and leave the second one alone: computers log in to /api/device/ with their own tokens
  hub:
    kind: HTTPRoute
    forceRename: {{ $full }}
    parentRefs: {{- toYaml (required "httpRoute.parentRefs is required" $v.httpRoute.parentRefs) | nindent 6 }}
    hostnames: [{{ include "talaria.host" . | quote }}]
    rules:
      # Each chat streams as server-sent events; the hub sends a comment every 15 s and the browser resumes
      - matches:
          - path: {type: PathPrefix, value: /}
        backendRefs:
          - {identifier: hub, port: {{ $v.service.port }}}
        timeouts:
          request: 24h
  device:
    kind: HTTPRoute
    forceRename: {{ $full }}-device
    parentRefs: {{- toYaml $v.httpRoute.parentRefs | nindent 6 }}
    hostnames: [{{ include "talaria.host" . | quote }}]
    rules:
      # Device connections are long-lived WebSockets with their own pings
      - matches:
          - path: {type: PathPrefix, value: /api/device/}
        backendRefs:
          - {identifier: hub, port: {{ $v.service.port }}}
        timeouts:
          request: 0s
  {{- end }}
{{- end -}}
