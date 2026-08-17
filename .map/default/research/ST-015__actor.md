{
  "status": "OK",
  "confidence": 0.9,
  "search_stats": {
    "files_scanned": 7,
    "total_matches_found": 9,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/default/spec_default.md",
      "lines": [
        1,
        150
      ],
      "relevance": "Разделы Security Boundaries, Out of Scope и Verified Runtime Assumptions — прямой источник для README: честная формулировка про hostPath, список Non-goals и восемь подтверждённых на стенде фактов."
    },
    {
      "path": ".map/wayfind/cpu-idle-tiers/resolutions/T-001.md",
      "lines": [
        1,
        60
      ],
      "relevance": "Ортогональность тиров и уточнённая таблица блокировок: idle запирает только cpu.weight, burst — только понижение квоты. Крест-накрест блокировок нет — в деке это надо давать в уточнённой форме."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-010.md",
      "lines": [
        1,
        67
      ],
      "relevance": "Риск для сообщения доклада: репозиторий про поды без CPU limits, а burst — про поды, у которых лимит есть. Снимается рамкой Maximum/Yield/Progress, поэтому порядок изложения в README имеет значение."
    },
    {
      "path": "hack/stand-probe.sh",
      "lines": [
        1,
        150
      ],
      "relevance": "Скрипт, воспроизводящий четыре ядерных факта одной командой — на него README обязан ссылаться как на способ проверить заявления самому."
    }
  ],
  "findings": {
    "framing_matters": "Репозиторий заявлен как «поды без CPU limits», а burst-тир — про поды, у которых лимит есть. Без рамки Maximum/Yield/Progress это читается как самопротиворечие. Порядок изложения — часть содержания.",
    "vpa_warning_is_measured": "Исключение обоих тиров из VPA — не осторожность: любая запись cpu.weight при cpu.idle=1 даёт EINVAL, а это ровно путь in-place resize. Понижение квоты ниже burst — тоже EINVAL.",
    "honest_security": "hostPath /sys/fs/cgroup на запись под uid 0 — де-факто право писать cgroup любого пода ноды. Измерено: контейнер с отброшенными capabilities это сделал. Формулировка про минимальные привилегии здесь была бы враньём.",
    "forbidden_claims": "Ни числового порога задержки применения (не измерен), ни утверждений про эффект cpu.max.burst на p99 (не измерен), ни экономии нод.",
    "supported_matrix": "cgroup v2 unified, ядро 5.15+, systemd — supported. cgroupfs — experimental, не покрыт ни e2e, ни стендом. cgroup v1 и hybrid — не поддерживаются, там нет cpu.idle.",
    "kind_answer": "Open Question 1 закрыт: kind нестит kubepods под kubelet.slice, поэтому e2e-гейт проверяет fail-safe поведение, а позитивный сценарий закрыт стендовым скриптом. Это стоит честно назвать в README, а не умолчать."
  }
}
