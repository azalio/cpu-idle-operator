{
  "status": "OK",
  "confidence": 0.9,
  "search_stats": {
    "files_scanned": 8,
    "total_matches_found": 9,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-011.md",
      "lines": [
        1,
        48
      ],
      "relevance": "Стратегия реконсиляции: событийный informer плюс редкий resync 60 с как страховка, а не механизм работы. Тесного цикла в 2 с, как в python-прототипе, не делать: за 120 с наблюдения на стенде дрейфа не было ни разу. Три исхода сравнения desired и actual, счётчик дрейфа как сигнал «нас кто-то переписывает»."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-008.md",
      "lines": [
        1,
        34
      ],
      "relevance": "Контроллера нет: без CRD ему нечего реконсайлить. controller-runtime берём как библиотеку, Manager и leader election не поднимаем — DaemonSet по определению один на ноду, гонки двух агентов за файл нет."
    },
    {
      "path": ".map/default/spec_default.md",
      "lines": [
        1,
        150
      ],
      "relevance": "AC-12: аннотация, добавленная или снятая на уже живом поде, обязана обрабатываться так же, как заданная при создании. Реализация, слушающая только Add, обязана этот критерий провалить — критерий добавлен ревьюером спеки именно потому, что такая реализация формально прошла бы AC-1."
    },
    {
      "path": "internal/apply/apply.go",
      "lines": [
        1,
        150
      ],
      "relevance": "Готовый Applier из ST-007 и Revert из ST-008: Reconciler обязан звать их, а не дублировать логику записи. ErrCgroupGone внутри уже даёт тихий возврат."
    }
  ],
  "findings": {
    "update_path_is_the_trap": "AC-12 существует потому, что реализация, подписанная только на Add, проходит AC-1 и тихо ломает kubectl annotate на живом поде. Это поймал ревьюер спеки, а не тесты.",
    "resync_is_insurance": "60 с — страховка от неизвестного писателя, а не механизм. Дрейф на resync инкрементит отдельный счётчик: устойчиво ненулевое значение означает, что мы кого-то не нашли.",
    "idempotence": "INV-6: совпадение desired и actual даёт ноль записей, ноль Event и ни одной строки лога уровня Info. Логировать «всё в порядке» на каждом проходе по каждому поду — это шум, который скроет настоящий сигнал.",
    "no_manager": "HC-4: Manager из controller-runtime и leader election не поднимать. DaemonSet один на ноду, ноды не пересекаются по cgroup-дереву.",
    "delete_is_not_error": "Delete-событие: каталог, скорее всего, уже исчез. Это ErrCgroupGone, который слой apply уже трактует как тихий возврат.",
    "reuse_apply": "Reconciler обязан звать готовые Apply и Revert, а не повторять логику записи и трассировки."
  }
}
