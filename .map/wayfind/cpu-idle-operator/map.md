<!-- DO NOT EDIT — regenerated from state.json by wayfind_runner.py. Manual edits will be overwritten. -->
# Wayfinding Map: cpu.idle operator (cpu.azalio.net)

- **Slug:** `cpu-idle-operator`
- **Status:** handed_off
- **Map ID:** `2865488bdc8f40f2b9cf94f4ba804535`
- **Revision:** 44

## Destination

Production-grade Kubernetes-оператор в API-группе cpu.azalio.net на Go/kubebuilder: node-agent ставит cpu.idle=1 на pod-cgroup по аннотации/политике, реконсайлит lifecycle (kubelet restart, QoS-slice rewrite, idle->0 восстановление cpu.weight), отдаёт метрики. Публикуется как публичный репозиторий к докладу про cpu.idle (CFP ~октябрь 2026), но качество — эксплуатационное, не демо.

## Notes

_None._

## Decisions so far

- **T-001** Prior art: Koordinator, KEP #5904, PR #136458 — Koordinator и upstream PR #136458 пишут cpu.idle на QoS-slice и упираются в EINVAL-коллизию #136025; наш per-pod путь живёт ниже kubelet reconciliation и её не воспроизводит — это и есть дифференциатор  ([resolution](resolutions/T-001.md))
- **T-002** Надёжное определение pod-cgroup path на ноде — Путь pod-cgroup вычисляем один раз через github.com/opencontainers/cgroups (ExpandSlice + pod<uid с подчёркиваниями>), не walk'ом; драйвер детектим по наличию /sys/fs/cgroup/kubepods.slice vs /sys/fs/cgroup/kubepods; cgroup v1 и hybrid — unsupported, отказываемся с явной ошибкой  ([resolution](resolutions/T-002.md))
- **T-003** Reference-архитектура operator + privileged DaemonSet — Каноничный layout: два бинаря cmd/manager + cmd/agent в одном kubebuilder-репо, раздельные SA и RBAC, агент — informer с fieldSelector spec.nodeName, kustomize, метрики только с bounded-лейблами (node/namespace/qos_class/result), тесты unit-на-fake-fs + envtest + kind e2e; откат cpu.idle при выходе агента отклонён, стартуем с одной версии CRD без conversion webhook  ([resolution](resolutions/T-003.md))
- **T-004** API-контракт: аннотация, CRD или оба — v1alpha1 — голая аннотация cpu.azalio.net/tier: idle на поде, без CRD, без admission webhook и без проверки прав: idle это самоограничение, навредить соседу им нельзя; CRD-политика откладывается в v1beta1  ([resolution](resolutions/T-004.md))
- **T-005** Стендовая проверка lifecycle под обоими cgroup-драйверами — Стенд подтвердил: per-pod cpu.idle=1 переживает 60с-цикл UpdateCgroups, создание соседнего пода и restart kubelet; idle 1->0 даёт вес 100 вместо request-derived 20, поэтому агент обязан восстанавливать вес сам; EINVAL при записи cpu.weight воспроизводится и на pod-cgroup (значит VPA/in-place resize на idle-поде упрётся в него); SYS_ADMIN не нужен, хватает hostPath rw + root; cgroupfs-драйвер не проверен  ([resolution](resolutions/T-005.md))
- **T-006** Кто восстанавливает cpu.weight при idle -> 0 — Вес восстанавливает агент сам, пересчитывая из requests.cpu по формуле kubelet (500m -> 20), строго после записи cpu.idle=0: kubelet pod-cgroup на живом поде не пишет, а ядро оставляет вес 100 вместо request-derived — иначе тихая пятикратная переплата доли CPU  ([resolution](resolutions/T-006.md))
- **T-007** Семантика cpu.idle при выходе и апгрейде агента — Агент при остановке ничего не откатывает — cgroup это desired state, а не сессия процесса: откат на SIGTERM превращал бы rolling update DaemonSet в инцидент по p99 прода и всё равно не срабатывал бы при SIGKILL/OOM; при старте агент идемпотентно реконсайлит всю ноду, осознанный откат вынесен в отдельный режим --revert-all  ([resolution](resolutions/T-007.md))
- **T-008** Нужен ли control-plane контроллер в v1alpha1 — Контроллер в v1alpha1 не нужен: без CRD, без webhook и с агентом, который сам смотрит поды своей ноды, ему нечего реконсайлить — один бинарь, один DaemonSet, controller-runtime как библиотека без kubebuilder-скаффолда, leader election не нужен; контроллер появляется вместе с CRD-политикой в v1beta1  ([resolution](resolutions/T-008.md))
- **T-009** Матрица supported окружений для v1alpha1 — Supported: cgroup v2 unified, ядро 5.15+, драйвер systemd (cgroupfs реализован, но не проверен и заявлен experimental), все три QoS-класса; CRI вообще не ось, потому что путь считается из pod UID без обращения к runtime; на неподдерживаемой ноде агент не крешлупит, а остаётся not-ready с метрикой причины и вешает событие на аннотированные поды  ([resolution](resolutions/T-009.md))
- **T-010** Burst-тир через cpu.max.burst — Burst — второй тир того же оператора через cpu.azalio.net/tier: burst и cpu.max.burst, применим только к подам с limits; измерена симметричная idle коллизия — burst>quota и понижение quota ниже burst дают EINVAL, то есть оба тира делают соседний knob частично неизменяемым и оба ломают in-place resize/VPA; эффект burst на p99 не измерен и не заявляется  ([resolution](resolutions/T-010.md))
- **T-011** Стратегия реконсиляции против других писателей cgroup — Событийный informer по подам своей ноды плюс редкий 60-секундный resync как страховка (тесный 2с-цикл прототипа не оправдан: дрейфа за 120с наблюдения не было); любое расхождение чиним, но с счётчиком и событием на поде; в живой pod-cgroup с VPA не воюем и QoS-слайсы не трогаем принципиально  ([resolution](resolutions/T-011.md))
- **T-012** Что в CI, а что только на стенде — Три уровня: unit на t.TempDir с конфигурируемым cgroupRoot и e2e на kind гейтят merge, envtest не нужен (нет CRD и контроллера); четыре факта, требующие настоящего kubelet (рестарт, 60с-цикл, EINVAL, потеря веса при idle->0), оформляются скриптом hack/stand-probe.sh — он же воспроизводимый артефакт к докладу; согласованность cgroup-вида внутри kind-ноды не проверена и это первый пункт к исполнению  ([resolution](resolutions/T-012.md))

## Frontier (resolve next, one non-research at a time)

_Empty._

## Blocked / claimed

_None._

## Fog of war (too vague to ticket yet)

_None._

## Out of scope

- Multi-tenancy-гейт исключён из v1alpha1: idle — self-harm, не эскалация привилегий — Решением по API-контракту гейт прав в v1alpha1 признан ненужным: аннотация tier: idle — самоограничение, обратной аннотации на повышение класса оператор не даёт, навредить соседу ею нельзя. Admission webhook в критическом пути создания подов всего кластера не окупается защитой от self-harm. Вопрос вернётся вместе с CRD-политикой в v1beta1. (from F-4)
