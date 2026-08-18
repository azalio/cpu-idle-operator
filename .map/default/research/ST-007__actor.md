{
  "status": "OK",
  "confidence": 0.93,
  "search_stats": {
    "files_scanned": 8,
    "total_matches_found": 11,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-tiers/resolutions/T-001.md",
      "lines": [
        1,
        60
      ],
      "relevance": "INV-7 и его обоснование: сначала ширина полосы (cpu.max.burst), затем порядок выбора (cpu.idle); при снятии — обратный порядок. Измерено, что оба тира сосуществуют на одной cgroup без взаимных блокировок."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.md",
      "lines": [
        1,
        108
      ],
      "relevance": "Измеренные ответы ядра: запись cpu.weight при cpu.idle=1 даёт EINVAL errno 22 для любого значения; переход idle 1->0 оставляет вес 100. Ловушка с потерей ошибки на закрытии дескриптора."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-010.md",
      "lines": [
        1,
        67
      ],
      "relevance": "burst > quota и понижение quota ниже burst дают EINVAL. Значение burst по умолчанию равно квоте. Burst при cpu.max = max неактивен."
    },
    {
      "path": ".map/default/code-review-002.md",
      "lines": [
        1,
        39
      ],
      "relevance": "Незакрытый хвост ST-002: guardWriteTarget восстанавливает корень из самого пути, а не сверяет с известным. Договорённость была закрыть это в ST-007, когда появится первый внешний вызывающий WriteKnob."
    }
  ],
  "findings": {
    "order_invariant": "INV-7: установка [cpu.max.burst, cpu.idle], снятие [cpu.idle, cpu.max.burst]. План должен быть наблюдаемым артефактом, иначе порядок проверяется только по конечному состоянию и регрессия проедет.",
    "burst_value": "Величина burst = квота из поля quota файла cpu.max того же pod-cgroup. При cpu.max = max квоты нет, запись не выполняется.",
    "measured_kernel_answers": "burst > quota -> EINVAL; понижение quota ниже burst -> EINVAL; запись cpu.weight при idle=1 -> EINVAL. Всё измерено, не выведено.",
    "quota_requires_all_limits": "Измерено на стенде 2026-08-17: квота на pod-cgroup появляется только когда limits.cpu задан у ВСЕХ контейнеров, включая простые init. Один контейнер без лимита -> cpu.max = max. ST-006 уже вычисляет это в BurstActive.",
    "two_silences": "AC-16 требует различать два молчания: незнакомое значение тира — ноль записей плюс один Event; исчезнувший каталог — ноль записей, ноль Event, nil error. Это разные вещи и путать их нельзя.",
    "guard_root_followup": "Хвост из ST-002: guardWriteTarget сейчас восстанавливает ожидаемый корень из самого переданного пути. Здесь появляется первый внешний вызывающий WriteKnob, и корень известен из конфигурации — уместно передавать его явно, закрыв дыру с поддельным вложенным pod-каталогом.",
    "snapshot_once": "Snapshot читать один раз в начале Apply. Повторное чтение между записями открывает окно гонки и делает план невоспроизводимым."
  }
}
