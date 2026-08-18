# Task Plan: cpu-idle-operator v1alpha1 — node-agent для cgroup v2 CPU-тиров

## Overview
- Goal: собрать production-grade node-agent, который по аннотациям `cpu.azalio.net/tier: idle` и `cpu.azalio.net/burst` управляет `cpu.idle`, `cpu.weight` и `cpu.max.burst` на pod-cgroup, никогда не трогая QoS-слайсы, и публикуется как репозиторий к докладу.
- Source spec: .map/default/spec_default.md
- Source wayfind: .map/wayfind/cpu-idle-operator/handoff.md, .map/wayfind/cpu-idle-tiers/handoff.md
- Mode: standard (research и архитектура закрыты wayfind-картами, поэтому --deep не запускался)

## Subtasks

### ST-001: Bootstrap Go-модуль, константы ключей аннотаций и флаги агента
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** config
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 3
- **AAG Contract:** annotations+config packages -> Keys(TierKey, TierValueIdle, BurstKey) + ParseFlags(argv) -> единственный источник ключей и конфигурации; go build ./... собирает ровно один бинарь cmd/agent без CRD, webhook и leader election
- **Test Strategy:** {'unit': 'internal/annotations/keys_test.go::TestVC2SingleAnnotationSource (обход исходников на литерал ключа), internal/config/flags_test.go::TestVC3DefaultsAndOverrides', 'integration': 'N/A', 'e2e': 'N/A', 'scenario_dimensions': {'happy_path': 'Парсинг флагов по умолчанию даёт /sys/fs/cgroup и resync 60s', 'error': 'Пустой --node-name и пустой env NODE_NAME дают явную ошибку старта, а не молчаливое watch по всем нодам', 'edge_case': 'test_vc1_no_forbidden_components ловит появление второго main-пакета', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [HC-4]: во всём дереве ровно один пакет main (cmd/agent/main.go); go.mod не содержит зависимостей на apiextensions/CRD-генераторы, в коде нет вызовов webhook.NewServer и нет LeaderElection: true (проверяется grep-тестом test_vc1_no_forbidden_components)
  - VC2 [CCR-2]: строковый литерал cpu.azalio.net/ встречается ровно в одном файле internal/annotations/keys.go; тест test_vc2_single_annotation_source обходит ./... и падает при втором вхождении
  - VC3: go build ./... и go vet ./... проходят на чистом клоне без сети сверх go mod download
- **Dependencies:** []

### ST-002: Реализовать слой cgroup: вычисление pod-cgroup path и запись knob с проверкой Close
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** medium | **Complexity:** 6
- **AAG Contract:** cgroup package -> PodCgroupPath(root, driver, qos, uid) + WriteKnob(dir, name, value) -> путь совпадает со снятым на стенде эталоном; запись вне pod-cgroup отвергается, ошибка Close не теряется, ENOENT возвращается как ErrCgroupGone
- **Test Strategy:** {'unit': 'internal/cgroup/path_test.go::TestVC5PodCgroupPathTable, internal/cgroup/knob_test.go::TestVC2CloseErrorSurfaced, ::TestVC3WriteTargetGuard, ::TestVC1NoHardcodedRoot, ::TestVC4NoRuntimeAccess', 'integration': 'N/A на этом слое; фактическая проверка пути на настоящем ядре входит в hack/stand-probe.sh (ST-014)', 'e2e': 'N/A', 'scenario_dimensions': {'happy_path': 'Burstable под с UID с дефисами даёт эталонный systemd-путь; запись cpu.idle=1 читается обратно', 'error': 'Close вернул ошибку -> WriteKnob вернул ошибку; каталог отсутствует -> ErrCgroupGone, а не generic error', 'edge_case': 'Guaranteed (без уровня QoS в пути), BestEffort, cgroupfs-ветка, UID без дефисов', 'security': 'Попытка записи в QoS-слайс, в корень kubepods и в container scope отвергается (INV-1)'}}
- **Validation Criteria:**
  - VC1 [CCR-3]: каждая экспортированная функция пакета принимает корень параметром; ни одного вхождения литерала /sys/fs/cgroup в internal/cgroup (проверяется тестом test_vc1_no_hardcoded_root), все unit-тесты работают под t.TempDir()
  - VC2 [INV-3]: WriteKnob возвращает ненулевую ошибку, когда Write прошёл, а Close вернул ошибку (test_vc2_close_error_surfaced через файл на заполненном tmpfs или подменённый writer)
  - VC3 [INV-1]: WriteKnob отвергает с ErrNotPodCgroup путь, указывающий на QoS-слайс (kubepods-burstable.slice), на корень kubepods.slice и на container scope (*.scope) — три отдельных кейса в test_vc3_write_target_guard
  - VC4 [HC-3]: grep-тест test_vc4_no_runtime_access подтверждает отсутствие в internal/ строк /proc/, unix:///run/containerd и импортов k8s.io/cri-api
  - VC5: табличный тест пути 2 драйвера x 3 QoS x UID с дефисами; эталон systemd-ветки — путь, реально снятый на стенде: /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid с подчёркиваниями>.slice
- **Dependencies:** ['ST-001']

### ST-003: Реализовать environment gate: cgroup v2 unified, ядро 5.15+, детект драйвера
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** medium | **Complexity:** 5
- **AAG Contract:** envgate -> Check(cgroupRoot, uname) -> Result{Ready, Reason, Driver, Experimental}; v1/hybrid/ядро<5.15 дают Ready=false с конкретной причиной, и ни одна запись в cgroup не выполняется
- **Test Strategy:** {'unit': 'internal/envgate/gate_test.go::TestVC1CgroupVersionGate, ::TestVC2KernelFloor, ::TestVC3NoWritesWhenGateFailed, ::TestVC4CgroupfsExperimentalWarning', 'integration': 'N/A', 'e2e': 'Косвенно: kind-нода обязана пройти гейт, иначе e2e (ST-013) падает с внятной причиной', 'scenario_dimensions': {'happy_path': 'Чистая v2 + systemd + ядро 6.17 -> Ready, Driver=systemd', 'error': 'v1, hybrid, старое ядро — каждый со своей константой Reason', 'edge_case': 'kubepods отсутствует (пустая нода) -> Reason=kubepods_missing, отдельно от driver_unknown; cgroupfs -> experimental', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [HC-1]: фикстура чистой v2 даёт Ready=true; фикстура с <root>/cpu/ (v1) даёт Ready=false, Reason=cgroup_v1; фикстура с <root>/unified/ даёт Reason=cgroup_hybrid — три отдельных кейса в test_vc1_cgroup_version_gate
  - VC2 [HC-2]: uname 5.14.0 даёт Ready=false, Reason=kernel_too_old; 5.15.0 и 6.17.0-061700-generic дают Ready=true (test_vc2_kernel_floor)
  - VC3 [INV-5]: при Ready=false ни один вызов cgroup.WriteKnob не происходит — тест test_vc3_no_writes_when_gate_failed запускает путь применения поверх t.TempDir() и сверяет побайтовую неизменность дерева до и после
  - VC4 [SC-1]: детект cgroupfs даёт Driver=cgroupfs, Experimental=true и ровно одну строку лога уровня Warn со словом experimental (test_vc4_cgroupfs_experimental_warning)
- **Dependencies:** ['ST-001', 'ST-002']

### ST-004: Реализовать чистые функции QoS-класса и веса CPU по формуле kubelet
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 4
- **AAG Contract:** qos package -> ClassOf(spec) + RestoreWeight(spec) -> детерминированные чистые значения; 500m -> 20, без requests -> 1, status.qosClass используется только для сверки и лога
- **Test Strategy:** {'unit': 'internal/qos/qos_test.go::TestVC3QoSFromSpec, internal/qos/weight_test.go::TestVC1NoRequestWeightOne, ::TestVC2MeasuredPair500m20, ::TestWeightTableMultiContainer', 'integration': 'N/A', 'e2e': 'N/A', 'scenario_dimensions': {'happy_path': 'Burstable 500m -> класс Burstable, вес 20', 'error': 'Расхождение вычисленного класса со status.qosClass -> mismatch=true и текст для лога, без паники и без подмены значения', 'edge_case': 'BestEffort без requests -> вес 1; несколько контейнеров; init-контейнер с большим request; requests в целых CPU', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [AC-14]: под без requests.cpu даёт RestoreWeight == 1 (test_vc1_no_request_weight_one)
  - VC2: под с requests.cpu 500m даёт RestoreWeight == 20 — измеренная на стенде пара, зафиксирована как эталон (test_vc2_measured_pair_500m_20)
  - VC3: ClassOf вычисляет Guaranteed/Burstable/BestEffort из spec; при расхождении с непустым status.qosClass VerifyAgainstStatus возвращает mismatch=true, а ClassOf-значение не меняется; пустой status.qosClass расхождением не считается (test_vc3_qos_from_spec_status_only_verifies)
- **Dependencies:** ['ST-001']

### ST-005: Реализовать метрики с ограниченными лейблами и рекордер Event'ов на подах
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** observability
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 4
- **AAG Contract:** observe package -> NewRecorder(registry, eventRecorder, node) -> Applied/Reverted/Inactive/Rejected; каждая метрика только с bounded-лейблами, каждый вызов даёт пару метрика+Event
- **Test Strategy:** {'unit': 'internal/observe/metrics_test.go::TestVC1MetricFamiliesPresent, ::TestVC2ForbiddenLabelsRejected, internal/observe/recorder_test.go::TestVC3MetricAndEventAreAtomic', 'integration': "Проверка, что Event'ы реально доезжают до API-сервера, входит в e2e (ST-013)", 'e2e': 'N/A на этом уровне', 'scenario_dimensions': {'happy_path': 'Применение тира даёт +1 к cpi_tier_apply_total{result=applied} и Event TierApplied', 'error': 'Отказ записи даёт result=error и reason из ошибки, а не пустую строку', 'edge_case': 'Reason конечного словаря: неизвестный reason нормализуется в other, чтобы не взорвать кардинальность через текст ошибки ядра', 'security': 'test_vc2_forbidden_labels_rejected закрывает HC-5 — единственный способ утечки идентификаторов подов в метрики'}}
- **Validation Criteria:**
  - VC1 [AC-8]: scrape тестового реестра отдаёт все четыре семейства — число подов в каждом тире, применения по результату, счётчик расхождений resync и причину отказа окружения (test_vc1_metric_families_present)
  - VC2 [HC-5]: конструктор паникует/возвращает ошибку при попытке объявить метрику с лейблом pod_name, pod_uid или container_id; тест test_vc2_forbidden_labels_rejected перебирает все три и дополнительно обходит зарегистрированные метрики, проверяя, что фактические имена лейблов входят в allowlist
  - VC3: Recorder.Applied(pod, knob, result, reason) в одном вызове инкрементит счётчик и пишет ровно один Event с involvedObject = этот под (test_vc3_metric_and_event_are_atomic на fake EventRecorder)
- **Dependencies:** ['ST-001']

### ST-006: Реализовать разбор аннотаций тиров и вычисление желаемого состояния пода
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 4
- **AAG Contract:** tier package -> Desired(pod) -> State{IdleRequested, BurstRequested, BurstActive} + Notes; незнакомое значение и burst без limits дают ноту, а не ошибку; величина аннотации burst игнорируется
- **Test Strategy:** {'unit': 'internal/tier/desired_test.go::TestVC1BurstWithoutLimitIsNote, ::TestVC2BurstValueIgnored, ::TestVC3UnknownVsAbsentTier, ::TestVC4BothTiersIndependent', 'integration': 'N/A', 'e2e': 'N/A', 'scenario_dimensions': {'happy_path': 'tier=idle -> IdleRequested=true без нот', 'error': 'Ни один вход не приводит к ошибке — проверяется отдельным property-подобным перебором аннотаций', 'edge_case': 'Пустая строка в значении tier, регистр (Idle vs idle) трактуется как незнакомое значение, burst без limits, оба ключа сразу', 'security': 'N/A — любой владелец пода вправе выставить тир (Decisions Made #3), проверки прав здесь сознательно нет'}}
- **Validation Criteria:**
  - VC1 [AC-4]: под с cpu.azalio.net/burst и без limits.cpu даёт BurstRequested=true, BurstActive=false, ровно одну Note с кодом no_cpu_limit и nil error; вызывающий слой обязан превратить эту ноту в Event TierInactive (test_vc1_burst_without_limit_is_note_not_error)
  - VC2 [SC-2]: аннотация burst со значением 200000 обрабатывается ровно так же, как с пустым значением: числовое переопределение не парсится, State не несёт поля с величиной (test_vc2_burst_value_ignored)
  - VC3: значение tier=aggressive даёт IdleRequested=false и Note{unknown_tier_value}; отсутствие ключа tier не даёт никакой ноты — разница проверяется явно (test_vc3_unknown_vs_absent_tier)
  - VC4: под с обеими аннотациями сразу даёт IdleRequested=true и BurstRequested=true независимо друг от друга (test_vc4_both_tiers_independent)
- **Dependencies:** ['ST-001', 'ST-004', 'ST-005']

### ST-007: Реализовать упорядоченный применитель тиров: план записей, burst раньше idle, след на каждое изменение
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** high | **Complexity:** 8
- **AAG Contract:** apply.Applier -> Apply(ctx, pod) -> BuildPlan даёт упорядоченный список записей (burst раньше idle), исполняет его через cgroup.WriteKnob и на каждую запись зовёт observe.Recorder; исчезнувший каталог даёт тихий nil, незнакомое значение — Event без записей
- **Test Strategy:** {'unit': 'internal/apply/plan_test.go::TestVC2ApplyOrderIsBurstThenIdle, ::TestVC3RevertPlanOrderIsReversed, internal/apply/apply_test.go::TestVC1BurstEqualsQuota, ::TestVC4UnknownValueVsMissingCgroup, ::TestVC5EveryWriteLeavesTrace', 'integration': 'internal/apply/apply_integration_test.go — исполнение плана поверх реального дерева-фикстуры в t.TempDir() с проверкой содержимого файлов', 'e2e': 'AC-3 и AC-13 подтверждаются на настоящем ядре в hack/stand-probe.sh (ST-014); в kind — по возможности (ST-013)', 'scenario_dimensions': {'happy_path': 'Оба тира применяются в правильном порядке, значения на месте, следы записаны', 'error': 'EINVAL от ядра при записи burst выше квоты -> result=einval, Event, отсутствие бесконечного ретрая', 'edge_case': 'cpu.max == max (нет квоты), исчезнувший каталог, незнакомое значение тира, повторный Apply на уже применённом состоянии (ноль записей)', 'security': "Попытка применить тир к пути вне pod-cgroup отвергается guard'ом из ST-002 и даёт Event WriteRejected (INV-1)"}}
- **Validation Criteria:**
  - VC1 [AC-3]: под с аннотацией burst и limits.cpu, чей cpu.max содержит 100000 100000, получает cpu.max.burst == 100000, равный квоте (test_vc1_burst_equals_quota); при cpu.max == max 100000 квоты нет и запись не выполняется
  - VC2 [AC-13]: под с обеими аннотациями даёт зафиксированную последовательность записей ровно [cpu.max.burst, cpu.idle] — утверждение на журнале вызовов фейкового writer'а, а не только на конечных значениях файлов (test_vc2_apply_order_is_burst_then_idle)
  - VC3 [INV-7]: BuildPlan для снятия обоих тиров возвращает обратный порядок — cpu.idle раньше cpu.max.burst (test_vc3_revert_plan_order_is_reversed)
  - VC4 [AC-16]: два вида молчания различаются явно — значение tier=aggressive даёт ноль записей и ровно один Event TierValueUnknown; отсутствующий каталог pod-cgroup даёт ноль записей, ноль Event'ов и nil error (test_vc4_unknown_value_vs_missing_cgroup, два независимых assert-блока)
  - VC5 [CCR-1]: число инкрементов cpi_tier_apply_total и число Event'ов равно числу фактически выполненных записей — ни одного изменения cgroup без следа (test_vc5_every_write_leaves_trace)
- **Dependencies:** ['ST-002', 'ST-005', 'ST-006']

### ST-008: Реализовать снятие тира: cpu.idle=0 первым, затем вес, пересчитанный из текущего spec
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** high | **Complexity:** 7
- **AAG Contract:** apply.Applier -> Revert(ctx, pod, state) -> пишет cpu.idle=0, затем cpu.weight из qos.RestoreWeight(текущий spec), затем снимает cpu.max.burst; вес никогда не пишется при cpu.idle=1 и никогда не берётся из кеша
- **Test Strategy:** {'unit': 'internal/apply/revert_test.go::TestVC1RevertRestoresMeasuredPair, ::TestVC2NoWeightWriteWhileIdle, ::TestVC3RequestChangedWhileIdle, ::TestVC3NoCachedWeightField', 'integration': 'internal/apply/apply_integration_test.go — полный цикл apply -> revert поверх дерева в t.TempDir() с проверкой финальных значений', 'e2e': 'AC-2 проверяется в kind e2e (ST-013) чтением фактического cpu.weight после снятия аннотации', 'scenario_dimensions': {'happy_path': '500m -> снятие -> cpu.idle=0, cpu.weight=20', 'error': 'Запись cpu.idle=0 не удалась -> вес не пишется, result=error, Event; каталог исчез -> тихий пропуск', 'edge_case': 'Под без requests.cpu -> вес 1 (AC-14 через ST-004); requests изменился во время idle; снятие только одного из двух тиров', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [AC-2]: снятие ключа tier у пода с requests.cpu 500m даёт последовательность [cpu.idle=0, cpu.weight=20]; итоговый cpu.idle == 0 и cpu.weight == 20 читаются из фикстуры (test_vc1_revert_restores_measured_pair)
  - VC2 [INV-2]: ни одна запись cpu.weight не происходит, пока snapshot показывает cpu.idle == 1 — журнал вызовов проверяется на то, что индекс записи cpu.idle=0 строго меньше индекса записи cpu.weight; отдельный кейс: если запись cpu.idle=0 вернула ошибку, запись веса не выполняется вовсе (test_vc2_no_weight_write_while_idle)
  - VC3 [AC-15]: requests.cpu изменился с 500m на 2 пока под был в idle — восстановленный вес вычислен из текущего spec, а не из значения на момент входа; дополнительно структурный тест test_vc3_no_cached_weight_field через reflect проверяет отсутствие в Applier полей с кешем веса/requests
- **Dependencies:** ['ST-004', 'ST-007']

### ST-009: Реализовать informer по подам своей ноды и идемпотентный цикл реконсиляции с resync 60 с
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** high | **Complexity:** 7
- **AAG Contract:** agent.Reconciler -> Run(ctx) с informer(fieldSelector spec.nodeName, resync 60s) -> add/update/delete приводят состояние pod-cgroup в соответствие с аннотациями; повторный проход по неизменившемуся поду не производит ни записей, ни Event'ов
- **Test Strategy:** {'unit': 'internal/agent/reconciler_test.go::TestVC1AnnotatedPodGetsIdle, ::TestVC2LivePodAnnotationAddAndRemove, ::TestVC3ReconcileIsIdempotent, internal/agent/informer_test.go::TestVC4FieldSelectorScopesToNode', 'integration': 'Fake clientset (k8s.io/client-go/kubernetes/fake) + дерево cgroup-фикстуры в t.TempDir(): полный путь событие -> запись без настоящего кластера', 'e2e': 'AC-1 и AC-12 подтверждаются на настоящем кластере в kind e2e (ST-013)', 'scenario_dimensions': {'happy_path': 'Add пода с аннотацией -> cpu.idle=1; удаление аннотации -> cpu.idle=0 и вес восстановлен', 'error': 'Ошибка применения возвращает под в очередь с backoff и не роняет цикл; под исчез между событием и записью -> тихий пропуск', 'edge_case': 'Пересоздание пода (новый UID -> новый cgroup, тир применяется заново), resync без изменений, поток из N подов одновременно', 'security': 'fieldSelector ограничивает watch подами своей ноды — поверхность API-сервера сведена к одному informer, как заявлено в Security Boundaries'}}
- **Validation Criteria:**
  - VC1 [AC-1]: под с cpu.azalio.net/tier: idle, добавленный в fake clientset, приводит к записи cpu.idle=1 в его pod-cgroup под t.TempDir() в пределах разумного таймаута ожидания теста; числовой SLA не утверждается (test_vc1_annotated_pod_gets_idle)
  - VC2 [AC-12]: аннотация добавлена update-событием на УЖЕ существующем поде -> тир применён; затем аннотация снята update-событием -> тир снят; реализация, подписанная только на Add, обязана провалить оба assert'а (test_vc2_live_pod_annotation_add_and_remove)
  - VC3 [INV-6]: второй проход реконсиляции по неизменившемуся поду даёт ноль вызовов writer'а, ноль Event'ов и ноль записей лога уровня Info; cpi_resync_drift_total остаётся 0 (test_vc3_reconcile_is_idempotent)
  - VC4: informer создаётся с fieldSelector spec.nodeName=<node> — проверяется на перехваченных ListOptions fake clientset (test_vc4_field_selector_scopes_to_node)
- **Dependencies:** ['ST-003', 'ST-007', 'ST-008']

### ST-010: Собрать cmd/agent: поведение на непрошедшей гейт ноде, readiness и остановка без отката
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** medium | **Complexity:** 6
- **AAG Contract:** cmd/agent -> main() -> при пройденном гейте запускает Reconciler; при непройденном остаётся живым и not-ready с причиной, метрикой и Event'ами; на SIGTERM завершается, не изменив ни одного значения cgroup
- **Test Strategy:** {'unit': 'cmd/agent/main_test.go::TestVC1V1NodeStaysNotReady, ::TestVC3ShutdownWritesNothing, ::TestVC4ReadinessAfterCacheSync', 'integration': 'internal/agent/lifecycle_test.go::TestVC2RestartConvergesStopChangesNothing — полный цикл поверх fake clientset и дерева в t.TempDir() с побайтовым снимком до/после', 'e2e': "AC-5 частично подтверждается в kind: удаление пода DaemonSet'а не меняет cpu.idle работающих подов (ST-013)", 'scenario_dimensions': {'happy_path': 'Пройденный гейт -> readiness 200, реконсиляция работает', 'error': "v1-нода -> 503 с причиной, живой процесс, Event'ы на аннотированных подах, ноль записей", 'edge_case': 'SIGTERM во время обработки очереди; повторный старт после жёсткого завершения; гейт пройден, но kubepods ещё нет', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [AC-6]: на фикстуре ноды с cgroup v1 процесс продолжает работать (не выходит и не перезапускается), readiness-эндпоинт отдаёт 503 с текстом причины cgroup_v1, cpi_environment_gate_info{reason=cgroup_v1} == 1, и на каждый аннотированный под ноды повешен ровно один Event EnvironmentUnsupported (test_vc1_v1_node_stays_not_ready)
  - VC2 [AC-5]: снимок дерева cgroup-фикстуры до и после полного цикла старт -> применение -> SIGTERM -> повторный старт показывает, что остановка не изменила ни байта, а повторный старт привёл состояние в соответствие с аннотациями (test_vc2_restart_converges_stop_changes_nothing)
  - VC3 [INV-4]: путь завершения не содержит вызовов applier — тест test_vc3_shutdown_writes_nothing прогоняет SIGTERM с журналирующим writer'ом и требует пустой журнал; дополнительно grep-тест подтверждает, что Revert не вызывается ни из одного defer/signal-хендлера в cmd/
  - VC4: readiness отдаёт 200 только после успешного гейта и первичной синхронизации кеша informer'а
- **Dependencies:** ['ST-003', 'ST-005', 'ST-009']

### ST-011: Реализовать одноразовый режим --revert-all
- **Status:** pending
- **Expected Diff Size:** small
- **Concern Type:** runtime
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 4
- **AAG Contract:** agent -> RunRevertAll(ctx, cfg) -> снимает cpu.idle и cpu.max.burst со всех подов ноды в порядке INV-2/INV-7, восстанавливает веса, печатает таблицу результатов и выходит с кодом 0 при полном успехе
- **Test Strategy:** {'unit': 'internal/agent/revertall_test.go::TestVC1RevertAllClearsNode, ::TestVC2PartialFailureNonzeroExit, ::TestVC3RevertAllIsOneshot', 'integration': 'Fake clientset + дерево фикстуры в t.TempDir()', 'e2e': 'N/A (не входит в блокирующий гейт; проверяется вручную на стенде)', 'scenario_dimensions': {'happy_path': 'Три пода с разными тирами -> всё снято, exit 0', 'error': 'EINVAL на одном поде -> остальные обработаны, exit != 0', 'edge_case': 'Нода без аннотированных подов -> exit 0 и пустая таблица; под удалён во время прохода -> ErrCgroupGone не влияет на код возврата', 'security': 'N/A'}}
- **Validation Criteria:**
  - VC1 [AC-7]: на фикстуре ноды с тремя подами (только idle, только burst, оба) запуск с --revert-all снимает оба тира со всех, восстанавливает веса из текущих spec и завершается кодом 0 (test_vc1_revert_all_clears_node_exit_zero)
  - VC2: при ошибке отката хотя бы одного пода код возврата ненулевой, а остальные поды всё равно обработаны — режим не останавливается на первой ошибке (test_vc2_partial_failure_nonzero_exit)
  - VC3: --revert-all не поднимает informer и сервер метрик — тест проверяет, что порт метрик свободен и watch к fake clientset не открывался (test_vc3_revert_all_is_oneshot)
- **Dependencies:** ['ST-008', 'ST-010']

### ST-012: Добавить kustomize-манифесты DaemonSet и RBAC без privileged и SYS_ADMIN
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** infra
- **One Logical Step:** true
- **Risk:** medium | **Complexity:** 5
- **AAG Contract:** config/base kustomization -> kustomize build -> DaemonSet с hostPath /sys/fs/cgroup rw и uid 0, без privileged и SYS_ADMIN, плюс RBAC ровно на чтение подов и создание событий; kubectl apply --dry-run проходит
- **Test Strategy:** {'unit': 'N/A (YAML); статическая проверка вынесена в hack/check-manifests.sh', 'integration': 'hack/check-manifests.sh: kustomize build + grep-гейты по HC-6 и RBAC + kubectl apply --dry-run=client', 'e2e': 'Применение манифестов в kind — часть ST-013 (дешёвая защита от рассинхрона кода и YAML)', 'scenario_dimensions': {'happy_path': 'kustomize build рендерит DaemonSet и RBAC, dry-run проходит', 'error': 'Добавление privileged: true в манифест роняет hack/check-manifests.sh — негативный кейс проверяется на временной копии', 'edge_case': 'Overlay для kind (образ с локальным тегом, imagePullPolicy: Never)', 'security': 'HC-6 + минимальный RBAC — оба гейта автоматические, формулировка про фактические привилегии дублируется в README (ST-015)'}}
- **Validation Criteria:**
  - VC1 [HC-6]: kustomize build config/base не содержит ни privileged: true, ни SYS_ADMIN; присутствуют runAsUser: 0, capabilities.drop: [ALL], allowPrivilegeEscalation: false и hostPath /sys/fs/cgroup с readOnly: false — проверяется скриптом hack/check-manifests.sh в CI (test_vc1_no_privileged_no_sysadmin)
  - VC2: ClusterRole даёт ровно get/list/watch на pods и create/patch на events; любой лишний ресурс или глагол ловится тем же скриптом (test_vc2_rbac_is_minimal)
  - VC3: kustomize build config/base | kubectl apply --dry-run=client -f - проходит без ошибок, и все три примера из config/samples валидны (test_vc3_manifests_apply_dry_run)
- **Dependencies:** ['ST-010']

### ST-013: Добавить kind e2e-гейт и CI-workflow (provisional: зависит от Open Question 1)
- **Status:** provisional
- **Expected Diff Size:** medium
- **Concern Type:** tests
- **One Logical Step:** true
- **Risk:** high | **Complexity:** 8
- **AAG Contract:** test/e2e -> kind up + kustomize apply + annotated pod -> фактическое значение cpu.idle в pod-cgroup ноды равно 1, после снятия аннотации равно 0 с восстановленным весом; pre-flight сначала доказывает, что агент и kubelet видят один и тот же путь
- **Test Strategy:** {'unit': 'N/A — уровень по определению кластерный', 'integration': 'hack/check-manifests.sh уже покрывает рендер; здесь проверяется применение манифестов в живом kind', 'e2e': 'test/e2e/preflight_test.go::TestVC2KindCgroupViewConsistency, test/e2e/e2e_test.go::TestVC1KindApplyAndRevert (тег build e2e, запуск make e2e)', 'scenario_dimensions': {'happy_path': 'Аннотированный под получает cpu.idle=1, снятие аннотации возвращает 0 и вес', 'error': 'Расхождение cgroup-вида в kind даёт падение с явной причиной, а не flaky-тест; недоступный cpu.max.burst даёт skip с причиной, а не молчаливый проход', 'edge_case': 'Аннотация добавлена на уже живом поде (перекрёстная проверка AC-12 на настоящем кластере)', 'security': 'Проверяется, что DaemonSet в kind стартует без privileged и SYS_ADMIN — то есть HC-6 держится не только на бумаге'}}
- **Validation Criteria:**
  - VC1 [AC-10]: в kind развёрнутый DaemonSet применяет tier: idle к аннотированному поду — тест читает фактическое значение cpu.idle из pod-cgroup внутри ноды (kubectl exec / docker exec, не из логов агента) и получает 1; после удаления аннотации читает 0 и восстановленный request-derived вес (test_vc1_kind_apply_and_revert)
  - VC2: pre-flight сравнивает путь, вычисленный агентом, с путём, который реально существует в ноде kind, и при расхождении падает с текстом, называющим Open Question 1; результат прогона фиксируется в README как ответ на вопрос (test_vc2_kind_cgroup_view_consistency)
  - VC3: CI-workflow блокирует merge на go vet, линтере, unit-тестах, hack/check-manifests.sh и e2e; отдельный шаг проверяет, что раннер на cgroup v2, и падает с явной причиной, если нет
- **Dependencies:** ['ST-012']

### ST-014: Написать hack/stand-probe.sh: таблица четырёх ядерных фактов с ненулевым кодом при расхождении
- **Status:** complete
- **Expected Diff Size:** medium
- **Concern Type:** tests
- **One Logical Step:** true
- **Risk:** medium | **Complexity:** 5
- **AAG Contract:** hack/stand-probe.sh -> запуск на ноде с настоящим kubelet -> таблица проверка/ожидалось/получилось по четырём ядерным фактам плюс комбинации тиров; ненулевой код при любом расхождении, namespace убран
- **Test Strategy:** {'unit': 'shellcheck hack/stand-probe.sh в CI + smoke-запуск с --dry-run на раннере (печатает план проверок, ничего не создаёт)', 'integration': 'N/A', 'e2e': 'Ручной и ночной прогон на стенде (нода с настоящим kubelet); merge не блокирует', 'scenario_dimensions': {'happy_path': 'Все пять строк совпали -> таблица и код 0', 'error': 'Расхождение хотя бы в одной строке -> ненулевой код и подсветка строки; отсутствие kubectl/sudo -> внятный отказ', 'edge_case': 'Нода без cgroup v2, нода с cgroupfs-драйвером (скрипт сообщает, что ветка не покрыта), прерывание по Ctrl-C с уборкой namespace', 'security': 'Скрипт пишет только в cgroup подопытного пода, созданного им самим, и удаляет его за собой'}}
- **Validation Criteria:**
  - VC1 [AC-9]: скрипт печатает таблицу со столбцами проверка / ожидалось / получилось по всем четырём фактам и возвращает ненулевой код, если хотя бы одна строка разошлась; вручную проверяется подменой ожидаемого значения — код возврата обязан стать ненулевым
  - VC2: пятая строка таблицы покрывает Unverified Runtime Assumption — комбинацию обоих тиров на живом pod-cgroup под управлением kubelet (рестарт и цикл реконсиляции)
  - VC3: shellcheck проходит без предупреждений; на ноде без kubelet или без cgroup v2 скрипт отказывается работать с внятным сообщением, а не падает на середине; trap удаляет созданный namespace на любом выходе
- **Dependencies:** []

### ST-015: Написать README: рамка Maximum / Yield / Progress, матрица supported, предупреждение про VPA
- **Status:** pending
- **Expected Diff Size:** medium
- **Concern Type:** docs
- **One Logical Step:** true
- **Risk:** low | **Complexity:** 3
- **AAG Contract:** README.md -> описывает тиры через Maximum/Yield/Progress, матрицу supported и исключение из VPA -> человек на ревью подтверждает объяснение по существу; hack/check-readme-keys.sh дополнительно сверяет ключи аннотаций с константами и ловит незамеренные утверждения
- **Test Strategy:** {'unit': 'hack/check-readme-keys.sh — сверка ключей аннотаций с константами и проверка отсутствия запрещённых утверждений (числовой SLA, эффект на p99)', 'integration': 'N/A', 'e2e': 'N/A', 'scenario_dimensions': {'happy_path': 'README проходит ручное ревью и автоматическую сверку ключей', 'error': 'Расхождение ключа в README с константой роняет CI', 'edge_case': 'Появление в README числа секунд рядом со словом задержка ловится списком запрещённых формулировок', 'security': 'Раздел безопасности обязан присутствовать и называть hostPath rw + uid 0 своим именем — отдельный пункт ручного чеклиста'}}
- **Validation Criteria:**
  - VC1 [AC-11]: README объясняет оба тира через рамку Maximum / Yield / Progress, содержит матрицу supported окружений и явное предупреждение об исключении обоих тиров из VPA. Критерий закрывается ревью человека, а не грепом: пункт вносится в чеклист PR как ручная проверка, потому что грепом ключевых слов легко пройти с неверным объяснением
  - VC2: ключи аннотаций в README совпадают с константами из internal/annotations/keys.go — проверяется скриптом hack/check-readme-keys.sh в CI (автоматическая часть, дополняющая ручное ревью)
  - VC3: README не содержит ни одного числового порога задержки применения тира и ни одного утверждения об эффекте cpu.max.burst на p99 — обе величины не измерены; проверяется тем же скриптом по списку запрещённых формулировок
- **Dependencies:** ['ST-001', 'ST-012']

## Execution Order

```
ST-014  (независима, можно начинать сразу — стендовый скрипт)
ST-001 -> ST-002 -> ST-003
       -> ST-004 -+
       -> ST-005 -+-> ST-006 -> ST-007 -> ST-008 -> ST-009 -> ST-010 -> ST-011
                                                                    -> ST-012 -> ST-013 (provisional)
                                                                             -> ST-015
```

## Spec Coverage

- AC-1 -> ST-009
- AC-2 -> ST-008
- AC-3 -> ST-007
- AC-4 -> ST-006
- AC-5 -> ST-010
- AC-6 -> ST-010
- AC-7 -> ST-011
- AC-8 -> ST-005
- AC-9 -> ST-014
- AC-10 -> ST-013
- AC-11 -> ST-015
- AC-12 -> ST-009
- AC-13 -> ST-007
- AC-14 -> ST-004
- AC-15 -> ST-008
- AC-16 -> ST-007
- CCR-1 -> ST-007
- CCR-2 -> ST-001
- CCR-3 -> ST-002
- HC-1 -> ST-003
- HC-2 -> ST-003
- HC-3 -> ST-002
- HC-4 -> ST-001
- HC-5 -> ST-005
- HC-6 -> ST-012
- INV-1 -> ST-002
- INV-2 -> ST-008
- INV-3 -> ST-002
- INV-4 -> ST-010
- INV-5 -> ST-003
- INV-6 -> ST-009
- INV-7 -> ST-007
- SC-1 -> ST-003
- SC-2 -> ST-006

## Notes

### Что валидатор пометил и почему это оставлено как есть

- **Глубина зависимостей 8 при пороге 5.** Цепочка `ST-001 -> ST-004 -> ST-006 -> ST-007 -> ST-008 -> ST-009 -> ST-010 -> ST-011` отражает настоящий порядок: нельзя реконсайлить до того, как есть применитель, и нельзя делать `--revert-all` до того, как есть снятие тира. Уплощение здесь означало бы объявить ложный параллелизм. Оставлено осознанно.
- **Fan-in больше трёх у ST-002, ST-003, ST-007.** Это не свалка требований, а связные концерны: слой cgroup честно владеет INV-1, INV-3, HC-3 и CCR-3, потому что все четыре — про одно и то же место в коде. Дробить ради счётчика не стоит.
- **Предупреждения про affected_files.** Часть подзадач правит файлы, созданные более ранними (`go.mod` у ST-002, `cmd/agent/main.go` у ST-010 и ST-011, `Makefile` у ST-013). Валидатор видит только текущее пустое дерево и не знает про порядок — это ложные срабатывания, а не галлюцинации декомпозера.
- **Ключи SC-1 и SC-2 в coverage_map** — два soft-ограничения из спеки. В Requirements Index их нет по определению, поэтому валидатор их не узнаёт. Покрытие полезное, оставлено.

### Риски исполнения

1. **ST-013 provisional.** Пока не проверено, видит ли агент внутри kind-ноды тот же cgroup-путь, что kubelet этой ноды, существование блокирующего e2e-гейта под вопросом. Первым делом внутри этой подзадачи идёт preflight-проверка; если она не сходится — kind заменяется одноузловой нодой в CI, и причина фиксируется письменно, а не замалчивается.
2. **ST-007 и ST-008 — сердце корректности.** Именно там живут измеренные на стенде инварианты: порядок `cpu.idle=0` перед весом, порядок burst раньше idle, проверка ошибки на закрытии дескриптора. Ошибка здесь не падает, а тихо деградирует справедливость распределения CPU. Ревью этих двух подзадач должно быть отдельным и придирчивым.
3. **Задержка применения тира нигде не измерена.** Числовой SLA сознательно убран из AC-1. Не возвращать его в код, тесты или README до замера.
4. **ST-014 не зависит ни от чего** и проверяет реальное ядро. Разумно начать с неё параллельно с ST-001: она же артефакт к докладу, и она поймает регрессию окружения раньше, чем появится код.

### Что специально не планируется

CRD-политика и control-plane контроллер, admission webhook, поддержка cgroup v1, активное противодействие VPA, числовое переопределение burst и любые заявления об эффекте `cpu.max.burst` на p99 — всё это в разделе Out of Scope спеки и подзадач не имеет.
