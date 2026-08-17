{
  "status": "OK",
  "confidence": 0.95,
  "search_stats": {
    "files_scanned": 6,
    "total_matches_found": 7,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-002.md",
      "lines": [
        1,
        150
      ],
      "relevance": "Раздел «Правки после ревью», пункты 1-4: детект драйвера обязан идти по v2-путям (<root>/kubepods.slice против <root>/kubepods). Предложенная в первой редакции эвристика по /sys/fs/cgroup/cpu/kubepods.slice — это cgroup v1, на чистой v2-ноде такого каталога нет вовсе. Там же: hybrid-режим /sys/fs/cgroup/unified — это не v2, controller cpu остаётся на v1."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-009.md",
      "lines": [
        1,
        50
      ],
      "relevance": "Матрица supported и поведение на неподдерживаемой ноде: не крешлупить, остаться not-ready с метрикой причины и вешать событие на аннотированные поды. cgroupfs — experimental с предупреждением в логе, а не supported."
    },
    {
      "path": "hack/stand-probe.sh",
      "lines": [
        1,
        150
      ],
      "relevance": "Рабочая реализация того же гейта на shell, функция preflight: тип ФС через stat -fc %T (ожидается cgroup2fs), сравнение версии ядра через sort -V, детект драйвера по наличию kubepods.slice против kubepods, и явный отказ с кодом 2 вместо тихого продолжения."
    },
    {
      "path": "internal/cgroup/driver.go",
      "lines": [
        1,
        55
      ],
      "relevance": "Готовый из ST-002 тип Driver (systemd|cgroupfs) — гейт обязан возвращать именно его, а не свою копию перечисления."
    }
  ],
  "findings": {
    "stand_values": "Проверено на стенде 2026-08-16: ядро 6.17.0-061700-generic, stat -fc %T /sys/fs/cgroup даёт cgroup2fs, каталог /sys/fs/cgroup/kubepods.slice существует, /sys/fs/cgroup/cpu/ отсутствует.",
    "why_v1_detection_is_wrong": "Эвристика по /sys/fs/cgroup/cpu/kubepods.slice, предложенная в первой редакции исследования T-002, на нашем же стенде вернула бы «драйвер не определён»: каталога cpu/ на чистой v2 нет. Это была ошибка, пойманная при ревью, и повторять её нельзя.",
    "kernel_floor": "5.15 — версия, в которой cpu.idle появился для cgroup-entity в upstream. Сравнивать major.minor; формат release может нести суффикс вида -061700-generic, его надо отбрасывать.",
    "gate_is_pure": "Гейт возвращает решение и ничего не пишет. Это прямо INV-5: на непрошедшей гейт ноде агент не выполняет ни одной записи. Тест VC3 обязан проверять побайтовую неизменность дерева, а не только отсутствие ошибки.",
    "no_crashloop": "Решение T-009: на неподдерживаемой ноде процесс живёт, readiness остаётся неготовой, метрика несёт причину. Гейт лишь сообщает причину, решение о поведении принимает cmd/agent в ST-010."
  }
}
