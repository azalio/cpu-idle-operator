{
  "status": "OK",
  "confidence": 0.95,
  "search_stats": {
    "files_scanned": 8,
    "total_matches_found": 9,
    "results_truncated": false
  },
  "relevant_locations": [
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-002.md",
      "lines": [
        1,
        150
      ],
      "relevance": "Правила построения пути для обоих драйверов, разбор алгоритма systemd.ExpandSlice, таблица QoS-родителей и обоснование выбора github.com/opencontainers/cgroups вместо k8s.io/kubernetes. Раздел «Правки после ревью» отменяет предложенный там детект драйвера по v1-путям."
    },
    {
      "path": ".map/wayfind/cpu-idle-operator/resolutions/T-005.md",
      "lines": [
        1,
        108
      ],
      "relevance": "Эталонный путь, реально снятый на стенде для Burstable-пода, и значения knob: cpu.weight 20 из requests 500m, EINVAL на записи веса при cpu.idle=1, вес 100 после снятия idle."
    },
    {
      "path": "hack/stand-probe.sh",
      "lines": [
        1,
        150
      ],
      "relevance": "Рабочая реализация той же логики на shell: pod_cgroup_path с заменой дефисов на подчёркивания и отдельной веткой для Guaranteed; write_knob с явной проверкой кода возврата вместо разбора текста ошибки."
    },
    {
      "path": "internal/annotations/keys.go",
      "lines": [
        1,
        16
      ],
      "relevance": "Готовый из ST-001 единственный источник ключей аннотаций — пакет cgroup его не импортирует, но CCR-2 запрещает дублировать литерал где-либо ещё."
    }
  ],
  "findings": {
    "reference_path_from_stand": "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid с подчёркиваниями>.slice — совпал с вычисленным без поправок",
    "guaranteed_has_no_qos_level": "Для Guaranteed уровень QoS в пути отсутствует: kubepods.slice/kubepods-pod<uid>.slice",
    "library": "github.com/opencontainers/cgroups v0.0.9 уже в go.mod после ST-001; нужна функция systemd.ExpandSlice. k8s.io/kubernetes как зависимость не брать — монолит",
    "close_error_trap": "Ключевая ловушка, подтверждённая экспериментально в этой сессии: ошибка записи в cgroup-файл всплывает на закрытии дескриптора, а не на самой записи. В Python первые два прогона burst-пробы дали ложный успех именно из-за этого. В Go эквивалент — defer f.Close() без проверки ошибки. WriteKnob обязан проверять Close и отдавать его ошибку приоритетно",
    "einval_is_expected": "EINVAL при записи cpu.weight в idle-cgroup — не баг, а нормальный ответ ядра. Слой knob не должен его прятать: вызывающий код обязан различать EINVAL и прочие ошибки",
    "enoent_is_normal": "Под может исчезнуть между чтением из informer и записью. ENOENT на каталоге — штатная гонка, поэтому отдельный сентинел ErrCgroupGone, а не generic error",
    "guard_rationale": "INV-1 запрещает писать выше pod-cgroup. Запись в QoS-слайс — ровно та коллизия upstream issue 136025, из-за которой Koordinator и upstream PR получают EINVAL. Guard обязан отвергать kubepods.slice, kubepods-<qos>.slice и *.scope",
    "no_runtime_access": "HC-3: путь считается только из UID и QoS-класса. Ни /proc/<pid>/cgroup, ни CRI-сокета, ни containerd. Это же делает оператор независимым от runtime"
  }
}
