{
  "status": "OK",
  "confidence": 0.9,
  "search_stats": {
    "files_scanned": 6,
    "total_matches_found": 7,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.md",
      "lines": [
        1,
        108
      ],
      "relevance": "Измерено: SYS_ADMIN не нужен. Под с privileged false, allowPrivilegeEscalation false, capabilities drop ALL и runAsUser 0 успешно записал cpu.idle чужого пода через hostPath. Догадка исследования про SYS_ADMIN опровергнута экспериментом."
    },
    {
      "path": ".map/default/spec_default.md",
      "lines": [
        1,
        150
      ],
      "relevance": "Раздел Security Boundaries: hostPath /sys/fs/cgroup на запись — де-факто право менять cgroup любого пода на ноде. В README это надо называть своим именем, а не прятать за фразой про минимальные привилегии."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-008.md",
      "lines": [
        1,
        34
      ],
      "relevance": "RBAC агента крошечный: get/list/watch на pods (ограничено fieldSelector) и create/patch на events. Больше ничего. Leader election нет, значит и leases не нужны."
    },
    {
      "path": "internal/config/flags.go",
      "lines": [
        1,
        74
      ],
      "relevance": "Флаги, которые манифест обязан прокинуть: --cgroup-root, --node-name (из NODE_NAME), --resync-period, --metrics-addr, --health-addr."
    }
  ],
  "findings": {
    "no_sys_admin_measured": "HC-6 подкреплён экспериментом, а не рассуждением: контейнер с отброшенными capabilities и без privileged записал cpu.idle. Достаточно hostPath на rw и uid 0.",
    "honest_security_wording": "hostPath /sys/fs/cgroup на запись означает право менять cgroup любого пода на ноде. Это неизбежно для DaemonSet, но называть это «минимальными привилегиями» нельзя.",
    "rbac_minimal": "get/list/watch на pods, create/patch на events. Leases не нужны — leader election нет.",
    "node_name_fieldref": "NODE_NAME обязан приходить через fieldRef spec.nodeName: пустое имя ноды агент трактует как фатальную ошибку старта (ST-001), а не как watch по всем нодам.",
    "readonly_rootfs": "readOnlyRootFilesystem true совместим с записью в hostPath — том монтируется отдельно.",
    "distroless": "Один статический бинарь в distroless. Агент не запускает подпроцессов и не нуждается в shell."
  }
}
