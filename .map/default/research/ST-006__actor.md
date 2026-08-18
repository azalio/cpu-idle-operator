{
  "status": "OK",
  "confidence": 0.92,
  "search_stats": {
    "files_scanned": 6,
    "total_matches_found": 7,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-tiers/resolutions/T-001.md",
      "lines": [
        1,
        60
      ],
      "relevance": "Решение об ортогональности: два независимых ключа, под может нести оба одновременно. Idle и burst обрабатываются независимо, а не как значения одного тира. Там же измерение, подтверждающее, что cpu.idle и cpu.max.burst не конфликтуют."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-010.md",
      "lines": [
        1,
        67
      ],
      "relevance": "Burst применим только к подам с limits: без квоты копить нечего. Поведение — применяем, вешаем событие, что тир неактивен. Молча делать вид, что политика применилась, нельзя. Там же: величина burst не парсится в первой версии, чтобы не фиксировать формат раньше времени."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-004.md",
      "lines": [
        1,
        34
      ],
      "relevance": "Незнакомое значение ключа тира трактуется как отсутствие тира: no-op плюс событие, не ошибка. Ключ зарезервирован под будущие тиры, поэтому строгая валидация значения закрыла бы расширение."
    },
    {
      "path": "internal/annotations/keys.go",
      "lines": [
        1,
        16
      ],
      "relevance": "Единственный источник ключей: TierKey, TierValueIdle, BurstKey. Пакет tier обязан брать их отсюда, дублировать литерал запрещено (CCR-2)."
    }
  ],
  "findings": {
    "orthogonal": "idle и burst — независимые переключатели, не значения одного тира. Под может нести оба.",
    "unknown_value_is_note": "tier=aggressive — это отсутствие тира плюс нота, а не ошибка. Разница между «неизвестное значение» и «ключа нет вовсе» обязана быть видимой: в первом случае нота есть, во втором нет.",
    "burst_needs_limit": "Без limits.cpu квоты нет, cpu.max.burst неактивен. Ошибкой это не является, но молчать нельзя — нота, которую вызывающий превратит в Event TierInactive.",
    "burst_value_not_parsed": "SC-2: значение ключа burst игнорируется целиком, State не несёт поля с величиной. Это осознанная отсрочка формата, а не недоделка.",
    "pure_function": "Desired ничего не пишет и не читает cgroup. Ноты — данные, их транслирует в события вызывающий слой через observe.Recorder.",
    "reason_codes_exist": "В ST-005 уже заведён закрытый словарь TierApplyReason, включающий value_unknown и limits_cpu_missing. Коды нот должны с ним стыковаться, а не заводить параллельную систему имён."
  }
}
