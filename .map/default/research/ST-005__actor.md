{
  "status": "OK",
  "confidence": 0.9,
  "search_stats": {
    "files_scanned": 6,
    "total_matches_found": 8,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-003.md",
      "lines": [
        1,
        150
      ],
      "relevance": "Раздел «Правки после ревью», пункт 2: исследование само себе противоречило, объявив pod_name bounded-лейблом. У Deployment-подов имя содержит случайный суффикс и меняется при каждом рестарте — это тот же cardinality-взрыв, что и запрещённый рядом pod_uid. Итоговое правило: только node, namespace, qos_class, result, reason."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-011.md",
      "lines": [
        1,
        48
      ],
      "relevance": "Что именно надо считать: счётчик расхождений при resync, различение «применили» и «уже было верно», событие на поде при каждом фактическом изменении в обе стороны. Ненулевой счётчик расхождений означает, что нас кто-то переписывает."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-009.md",
      "lines": [
        1,
        50
      ],
      "relevance": "Поведение на неподдерживаемой ноде: метрика с лейблом причины плюс событие на каждом аннотированном поде. Отсюда метрика gate_info и Event EnvironmentUnsupported."
    },
    {
      "path": ".map/default/spec_default.md",
      "lines": [
        1,
        150
      ],
      "relevance": "CCR-1: каждое фактическое изменение cgroup сопровождается Event на поде и инкрементом метрики; изменение без следа запрещено. Отсюда объединяющий Recorder, чтобы нельзя было забыть половину."
    }
  ],
  "findings": {
    "forbidden_labels": "pod_name, pod_uid, container_id — запрещены жёстко (HC-5). Ловушка: pod_name интуитивно кажется ограниченным, но у Deployment-подов он содержит случайный суффикс.",
    "allowlist": "node, namespace, qos_class, tier, result, reason",
    "why_combined_recorder": "CCR-1 нельзя выполнить дисциплиной: рано или поздно кто-то инкрементит счётчик и забывает Event. Поэтому пара делается одним вызовом.",
    "controller_runtime_first_import": "Это первое место, где реально импортируется controller-runtime (record.EventRecorder). Monitor ST-001 предупреждал: controller-runtime прямо требует k8s.io/apiextensions-apiserver в своём go.mod, и после go get тест VC1 из ST-001, который грепает go.mod на подстроку apiextensions, может упасть. Если это произойдёт — это не нарушение HC-4 по существу (CRD мы не заводим), а слишком грубый греп-тест; сузить его до проверки собственного кода агента.",
    "no_high_cardinality_by_construction": "Проверять allowlist в конструкторе, а не только тестом: тест ловит то, что уже написано, конструктор — то, что напишут потом."
  }
}
