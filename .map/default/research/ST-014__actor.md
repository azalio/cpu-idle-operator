{
  "status": "OK",
  "confidence": 0.95,
  "search_stats": {
    "files_scanned": 6,
    "total_matches_found": 11,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.md",
      "lines": [
        1,
        108
      ],
      "relevance": "Протокол прямого измерения четырёх ядерных фактов: путь pod-cgroup для systemd-драйвера, вес 20 из requests 500m, EINVAL errno 22 на записи cpu.weight при cpu.idle=1, вес 100 после перехода idle 1->0, выживание 120 секунд и рестарта kubelet. Скрипт проверяет ровно эти значения."
    },
    {
      "path": ".map/wayfind/cpu-idle-tiers/resolutions/T-001.md",
      "lines": [
        1,
        60
      ],
      "relevance": "Измерение ортогональности: cpu.idle, cpu.max и cpu.max.burst сосуществуют, взаимных блокировок нет. Источник пятой строки таблицы (VC2): комбинацию проверить на живом pod-cgroup под kubelet, а не на голой cgroup."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.recon.md",
      "lines": [
        1,
        28
      ],
      "relevance": "Параметры стенда: ядро 6.17, Kubernetes 1.36.3, containerd 2.2.1, cgroupDriver systemd, чистая cgroup2fs. Отсюда ограничение дизайна: на worker нет kubectl."
    },
    {
      "path": ".map/default/spec_default.md",
      "lines": [
        75,
        215
      ],
      "relevance": "Блок Acceptance Criteria: AC-9 задаёт формат таблицы и ненулевой код при расхождении, AC-16 — разницу между Event и тихим пропуском."
    },
    {
      "path": ".map/default/blueprint.json",
      "lines": [
        1,
        200
      ],
      "relevance": "Контракт ST-014: VC1 формат и код возврата, VC2 пятая строка про комбинацию тиров, VC3 shellcheck и trap. Разрешённые файлы: hack/stand-probe.sh и hack/fixtures/probe-pod.yaml."
    }
  ],
  "findings": {
    "expected_values": {
      "pod_cgroup_path_systemd": "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid_underscored>.slice",
      "weight_from_500m": 20,
      "weight_after_idle_set": 1,
      "weight_after_idle_cleared": 100,
      "write_weight_while_idle": "EINVAL errno 22 для любого значения, включая текущее",
      "idle_survives_seconds": 120,
      "idle_survives_kubelet_restart": true,
      "burst_over_quota": "EINVAL",
      "lower_quota_below_burst": "EINVAL"
    },
    "design_constraint": "Скрипт исполняется на ноде: нужны /sys/fs/cgroup и systemctl restart kubelet. На типичном worker нет kubectl, поэтому обязателен режим пробы существующего пода по UID, без создания чего-либо.",
    "trap_to_avoid": "Буферизованная запись теряет ошибку: в Python она всплывает на close(); в shell 'echo 1 > file' внутри sh -c теряет код возврата и искажает сообщение. Проверять код возврата самой записи, текст не разбирать.",
    "not_covered": "Ветка cgroupfs-драйвера: ноды с ним нет, скрипт обязан сказать, что ветка не покрыта.",
    "method_note": "Research-агент не привлекался: значения получены эмпирически в текущей сессии."
  }
}
