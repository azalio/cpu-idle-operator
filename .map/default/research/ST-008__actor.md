{
  "status": "OK",
  "confidence": 0.94,
  "search_stats": {
    "files_scanned": 7,
    "total_matches_found": 8,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-006.md",
      "lines": [
        1,
        60
      ],
      "relevance": "Решение: вес восстанавливает агент сам, пересчитывая из requests.cpu, строго после cpu.idle=0. Источник истины — spec, не кеш: кеш теряется при рестарте агента, а idle-поды его переживают. Кеш допустим только как детектор расхождений."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.md",
      "lines": [
        1,
        108
      ],
      "relevance": "Измерено: переход cpu.idle 1->0 оставляет вес 100, а не request-derived 20. Запись cpu.weight при cpu.idle=1 отвергается ядром с EINVAL для любого значения, включая текущее."
    },
    {
      "path": "internal/apply/plan.go",
      "lines": [
        1,
        105
      ],
      "relevance": "Реализованный в ST-007 revert-план: порядок снятия [cpu.idle, cpu.max.burst]. ST-008 добавляет между ними восстановление веса."
    },
    {
      "path": "internal/qos/weight.go",
      "lines": [
        1,
        112
      ],
      "relevance": "RestoreWeight из ST-004 — чистая функция от текущего spec с kubelet-паритетом по sidecar. Именно её надо звать в момент снятия, а не запоминать результат заранее."
    }
  ],
  "findings": {
    "order_is_physical": "Порядок cpu.idle=0 раньше cpu.weight не стилистический: ядро физически отвергает запись веса при idle=1 с EINVAL. Обратный порядок невозможен.",
    "why_agent_restores": "Ядро при снятии idle оставляет вес 100 вместо request-derived 20 — измерено. Kubelet живой pod-cgroup не пишет, значит если агент не восстановит, под тихо получит впятеро большую долю CPU.",
    "no_cache": "AC-15: если requests.cpu изменился, пока под был в idle, восстанавливать надо НОВОЕ значение. Кеш на входе в idle даёт неверный результат и теряется при рестарте агента.",
    "failure_stops_chain": "Если запись cpu.idle=0 не удалась, вес писать нельзя вовсе — ядро всё равно отвергнет, а попытка создаст ложный след в метриках.",
    "trace_required": "CCR-1: каждая выполненная запись даёт метрику и Event через observe.Recorder, как в ST-007."
  }
}
