#!/usr/bin/env bash
#
# stand-probe.sh — проверка ядерных фактов, на которых стоит cpi-idle-operator.
#
# Четыре факта невозможно воспроизвести без настоящего ядра и настоящего kubelet,
# поэтому они не покрываются ни unit-тестами, ни kind e2e:
#
#   1. cpu.idle=1 на pod-cgroup переживает `systemctl restart kubelet`
#   2. переживает 60-секундный цикл qosContainerManager.UpdateCgroups и создание соседнего пода
#   3. запись cpu.weight в cgroup с cpu.idle=1 отвергается ядром
#   4. переход cpu.idle 1 -> 0 оставляет вес по умолчанию, а не request-derived
#
# Пятая проверка закрывает неподтверждённое допущение: комбинация обоих тиров
# (cpu.idle вместе с cpu.max.burst) на живом pod-cgroup под управлением kubelet.
#
# Скрипт исполняется НА НОДЕ: ему нужны /sys/fs/cgroup и systemctl.
# Если на ноде нет kubectl (обычная ситуация для worker), используйте режим
# --pod-uid: подопытные поды создаёт кто-то другой, а скрипт работает с готовыми.
# Этот режим НЕ пассивный — он пишет в cgroup переданного пода и не может проверить,
# что UID принадлежит именно подопытному поду. Подробности и риски — в --help.
#
# Коды возврата:
#   0 — все проверки сошлись
#   1 — хотя бы одна проверка разошлась с ожидаемым
#   2 — окружение или аргументы не позволяют выполнить проверку
#
set -euo pipefail

readonly CGROUP_ROOT="${CGROUP_ROOT:-/sys/fs/cgroup}"
readonly MIN_KERNEL="5.15"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly FIXTURE="${SCRIPT_DIR}/fixtures/probe-pod.yaml"

NAMESPACE="cpi-idle-probe"
NODE_NAME="$(hostname)"
IDLE_POD_UID=""
BURST_POD_UID=""
DRY_RUN=0
KEEP=0
RESTART_KUBELET=1
SETTLE_SECONDS=75

# Строки итоговой таблицы: "проверка|ожидалось|получилось|статус"
ROWS=()
FAILED=0
CREATED_NAMESPACE=0

usage() {
    cat <<'USAGE'
Использование: hack/stand-probe.sh [опции]

  --dry-run              напечатать план проверок и ожидаемые значения, ничего не создавая
  --namespace NAME       namespace для подопытных подов (по умолчанию cpi-idle-probe)
  --node NAME            имя ноды для nodeName (по умолчанию hostname)
  --pod-uid UID          не создавать ничего: мерить уже существующий под без limits
  --burst-pod-uid UID    то же для пода с limits (комбинация обоих тиров)
  --no-kubelet-restart   пропустить проверку рестарта kubelet
  --settle SECONDS       длительность окна наблюдения (по умолчанию 75, минимум 65)
  --keep                 не удалять созданный namespace после прогона
  -h, --help             эта справка

Режимы:
  Без --pod-uid скрипт сам создаёт namespace и подопытные поды — нужен kubectl на ноде.

  С --pod-uid (и, желательно, --burst-pod-uid) kubectl не нужен: поды создаёт кто-то другой.

  ВНИМАНИЕ: режим --pod-uid НЕ пассивный. Скрипт ПИШЕТ в cgroup переданного пода:
  выставляет cpu.idle=1, пробует записать cpu.weight, для burst-пода трогает cpu.max.burst,
  и только в конце возвращает cpu.idle=0. Всё это время (окно наблюдения плюс рестарт
  kubelet, около двух минут) под реально уступает CPU соседям.
  Проверить, что UID принадлежит именно подопытному поду, скрипт не может — kubectl
  в этом режиме недоступен по условию. Передавайте UID только того пода, который вам
  не жалко притормозить. За собой скрипт в этом режиме ничего не убирает.
USAGE
}

die() {
    printf 'stand-probe: %s\n' "$1" >&2
    exit 2
}

log() {
    printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$1" >&2
}

# --- аргументы ---------------------------------------------------------------

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        --namespace) NAMESPACE="${2:?--namespace требует значение}"; shift ;;
        --node) NODE_NAME="${2:?--node требует значение}"; shift ;;
        --pod-uid) IDLE_POD_UID="${2:?--pod-uid требует значение}"; shift ;;
        --burst-pod-uid) BURST_POD_UID="${2:?--burst-pod-uid требует значение}"; shift ;;
        --no-kubelet-restart) RESTART_KUBELET=0 ;;
        --settle) SETTLE_SECONDS="${2:?--settle требует значение}"; shift ;;
        --keep) KEEP=1 ;;
        -h|--help) usage; exit 0 ;;
        *) die "неизвестный аргумент: $1 (см. --help)" ;;
    esac
    shift
done

if [ "$SETTLE_SECONDS" -lt 65 ]; then
    die "--settle меньше 65 секунд не имеет смысла: цикл UpdateCgroups у kubelet равен 60"
fi

# --- вспомогательное ---------------------------------------------------------

# Вес cpu.weight, который kubelet выводит из requests.cpu, по его же формуле
# конвертации shares в cgroup v2:
#   shares = max(2, milliCPU * 1024 / 1000)
#   weight = 1 + ((shares - 2) * 9999) / 262142
# Знаменатель именно 262142, а не 262140: на 500m оба варианта дают 20 из-за
# целочисленного деления, поэтому ошибка здесь не проявилась бы наблюдаемо.
# Проверено на стенде: 500m -> 20.
expected_weight() {
    local milli="$1" shares
    shares=$(( milli * 1024 / 1000 ))
    [ "$shares" -lt 2 ] && shares=2
    printf '%d' $(( 1 + ( (shares - 2) * 9999 ) / 262142 ))
}

# Путь pod-cgroup для systemd-драйвера. Дефисы в UID заменяются подчёркиваниями.
pod_cgroup_path() {
    local uid="$1" qos="$2" escaped
    escaped="${uid//-/_}"
    case "$qos" in
        guaranteed) printf '%s/kubepods.slice/kubepods-pod%s.slice' "$CGROUP_ROOT" "$escaped" ;;
        *)          printf '%s/kubepods.slice/kubepods-%s.slice/kubepods-%s-pod%s.slice' \
                        "$CGROUP_ROOT" "$qos" "$qos" "$escaped" ;;
    esac
}

read_knob() {
    local dir="$1" knob="$2"
    if [ -r "${dir}/${knob}" ]; then
        tr -d '\n' < "${dir}/${knob}"
    else
        printf '<нет файла %s>' "$knob"
    fi
}

# Пишет значение и возвращает код ядра. Текст сообщения намеренно не разбирается:
# при потере кода возврата shell показывает ошибку не того класса, чем вернуло ядро.
write_knob() {
    local dir="$1" knob="$2" value="$3"
    sudo tee "${dir}/${knob}" >/dev/null 2>&1 <<<"$value"
}

# printf выравнивает по БАЙТАМ, а кириллица в UTF-8 занимает два байта на символ,
# из-за чего колонки разъезжаются. ${#s} в bash считает символы — выравниваем сами.
pad() {
    local s="$1" width="$2" n
    n=$(( width - ${#s} ))
    [ "$n" -lt 0 ] && n=0
    printf '%s%*s' "$s" "$n" ""
}

record() {
    local name="$1" expected="$2" got="$3" status
    if [ "$expected" = "$got" ]; then
        status="OK"
    else
        status="ПРОВАЛ"
        FAILED=1
    fi
    ROWS+=("${name}|${expected}|${got}|${status}")
    printf '  %s %s\n' "$(pad "$name" 44)" "$status" >&2
}

record_skipped() {
    ROWS+=("$1|$2|пропущено|ПРОПУСК")
    printf '  %s %s\n' "$(pad "$1" 44)" "ПРОПУСК" >&2
}

print_table() {
    local sep row n e g s
    sep="$(printf '%.0s-' {1..100})"
    printf '\n%s\n' "$sep"
    printf '%s | %s | %s | %s\n' "$(pad ПРОВЕРКА 44)" "$(pad ОЖИДАЛОСЬ 20)" "$(pad ПОЛУЧИЛОСЬ 20)" "СТАТУС"
    printf '%s\n' "$sep"
    for row in "${ROWS[@]}"; do
        IFS='|' read -r n e g s <<<"$row"
        printf '%s | %s | %s | %s\n' "$(pad "$n" 44)" "$(pad "$e" 20)" "$(pad "$g" 20)" "$s"
    done
    printf '%s\n' "$sep"
}

cleanup() {
    local code=$?
    if [ "$CREATED_NAMESPACE" -eq 1 ] && [ "$KEEP" -eq 0 ]; then
        log "убираю namespace ${NAMESPACE}"
        kubectl delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
    fi
    exit "$code"
}

# --- преflight ---------------------------------------------------------------

preflight() {
    local fstype kernel driver

    fstype="$(stat -fc %T "$CGROUP_ROOT" 2>/dev/null || true)"
    [ "$fstype" = "cgroup2fs" ] || die "нужна чистая cgroup v2: ${CGROUP_ROOT} имеет тип '${fstype:-неизвестен}'. В cgroup v1 файла cpu.idle не существует"

    kernel="$(uname -r | cut -d- -f1)"
    if [ "$(printf '%s\n%s\n' "$MIN_KERNEL" "$kernel" | sort -V | head -1)" != "$MIN_KERNEL" ]; then
        die "нужно ядро ${MIN_KERNEL} или новее, обнаружено ${kernel}: cpu.idle для cgroup-entity появился в ${MIN_KERNEL}"
    fi

    if [ -d "${CGROUP_ROOT}/kubepods.slice" ]; then
        driver="systemd"
    elif [ -d "${CGROUP_ROOT}/kubepods" ]; then
        die "обнаружен cgroupfs-драйвер. Эта ветка НЕ ПОКРЫТА проверкой: эталонных значений для неё не снимали. Проверка не выполнена — это не то же самое, что пройдена"
    else
        die "не найдено ни ${CGROUP_ROOT}/kubepods.slice, ни ${CGROUP_ROOT}/kubepods: похоже, на этой ноде нет запущенного kubelet"
    fi
    log "окружение: ядро ${kernel}, cgroup v2, драйвер ${driver}"

    sudo -n true 2>/dev/null || die "нужен беспарольный sudo: скрипт пишет в cgroup и перезапускает kubelet"

    if [ -z "$IDLE_POD_UID" ]; then
        command -v kubectl >/dev/null 2>&1 \
            || die "на этой ноде нет kubectl. Создайте поды откуда-нибудь ещё и передайте --pod-uid UID [--burst-pod-uid UID]"
        [ -r "$FIXTURE" ] || die "не найден фикстур-файл ${FIXTURE}"
    fi
}

# --- создание подов ----------------------------------------------------------

create_pods() {
    log "создаю namespace ${NAMESPACE} и подопытные поды на ноде ${NODE_NAME}"
    # Не идемпотентно намеренно: cleanup удаляет namespace без ожидания, и быстрый
    # повторный запуск застаёт его в Terminating. Отличаем это от расхождения проверки
    # явным кодом 2, иначе проблема окружения выглядела бы как провал эталона.
    kubectl create namespace "$NAMESPACE" >/dev/null 2>&1 \
        || die "не удалось создать namespace ${NAMESPACE} — возможно, он ещё удаляется после прошлого прогона. Подождите или задайте другой через --namespace"
    CREATED_NAMESPACE=1
    sed -e "s|__NAMESPACE__|${NAMESPACE}|g" -e "s|__NODE__|${NODE_NAME}|g" "$FIXTURE" \
        | kubectl apply -f - >/dev/null
    kubectl -n "$NAMESPACE" wait --for=condition=Ready pod/probe-idle pod/probe-burst --timeout=120s >/dev/null
    IDLE_POD_UID="$(kubectl -n "$NAMESPACE" get pod probe-idle -o jsonpath='{.metadata.uid}')"
    BURST_POD_UID="$(kubectl -n "$NAMESPACE" get pod probe-burst -o jsonpath='{.metadata.uid}')"
    log "probe-idle uid=${IDLE_POD_UID}"
    log "probe-burst uid=${BURST_POD_UID}"
}

# kubectl может быть установлен, но без рабочего kubeconfig — на worker это обычное дело.
# Проверяем именно достижимость API, а не наличие бинаря.
kubectl_usable() {
    command -v kubectl >/dev/null 2>&1 && kubectl get namespace >/dev/null 2>&1
}

# Возвращает код kubectl. Ошибку НЕ гасим: строка таблицы обязана уметь провалиться,
# иначе она сообщает об успехе, которого не было.
create_neighbour() {
    kubectl -n "$NAMESPACE" run probe-neighbour \
        --image=busybox:1.36 --restart=Never \
        --overrides="{\"spec\":{\"nodeName\":\"${NODE_NAME}\",\"containers\":[{\"name\":\"n\",\"image\":\"busybox:1.36\",\"command\":[\"sleep\",\"300\"],\"resources\":{\"requests\":{\"cpu\":\"200m\",\"memory\":\"16Mi\"}}}]}}" \
        >/dev/null 2>&1
}

# --- проверки ----------------------------------------------------------------

run_checks() {
    local idle_dir burst_dir want_weight quota got_weight rc neighbour_rc
    local kernel_default_weight=100

    idle_dir="$(pod_cgroup_path "$IDLE_POD_UID" burstable)"
    [ -d "$idle_dir" ] || die "нет каталога pod-cgroup: ${idle_dir}. Проверьте UID и QoS-класс пода"
    want_weight="$(expected_weight 500)"

    if [ "$CREATED_NAMESPACE" -eq 0 ]; then
        log "ВНИМАНИЕ: под создан не мной. Скрипт БУДЕТ ПИСАТЬ в ${idle_dir}"
        log "ВНИМАНИЕ: под уступит CPU примерно на две минуты, пока идёт прогон"
    fi

    # 1. Базовая линия: формула пути сошлась, вес выведен из requests.
    record "путь pod-cgroup и вес из requests 500m" \
        "idle=0 weight=${want_weight}" \
        "idle=$(read_knob "$idle_dir" cpu.idle) weight=$(read_knob "$idle_dir" cpu.weight)"

    # 2. Установка idle: ядро само роняет вес в 1.
    write_knob "$idle_dir" cpu.idle 1 || die "не удалось записать cpu.idle в ${idle_dir}"
    record "cpu.idle=1 сбрасывает вес в 1" \
        "idle=1 weight=1" \
        "idle=$(read_knob "$idle_dir" cpu.idle) weight=$(read_knob "$idle_dir" cpu.weight)"

    # 3. Ядро запрещает менять вес idle-группы. Смотрим код возврата, не текст.
    rc=0
    write_knob "$idle_dir" cpu.weight "$want_weight" || rc=$?
    record "запись cpu.weight при cpu.idle=1 отвергнута" \
        "запись отвергнута" \
        "$([ "$rc" -ne 0 ] && echo "запись отвергнута" || echo "запись прошла")"

    # 4. Комбинация обоих тиров на живом pod-cgroup (неподтверждённое допущение).
    if [ -n "$BURST_POD_UID" ]; then
        burst_dir="$(pod_cgroup_path "$BURST_POD_UID" burstable)"
        if [ -d "$burst_dir" ]; then
            quota="$(read_knob "$burst_dir" cpu.max | cut -d' ' -f1)"
            # Порядок как в INV-7: сначала ширина полосы, потом порядок выбора.
            write_knob "$burst_dir" cpu.max.burst "$quota" || die "не удалось записать cpu.max.burst"
            write_knob "$burst_dir" cpu.idle 1 || die "не удалось записать cpu.idle для burst-пода"
            record "оба тира сосуществуют на pod-cgroup" \
                "idle=1 burst=${quota}" \
                "idle=$(read_knob "$burst_dir" cpu.idle) burst=$(read_knob "$burst_dir" cpu.max.burst)"
        else
            burst_dir=""
            record_skipped "оба тира сосуществуют на pod-cgroup" "idle=1 burst=quota"
        fi
    else
        burst_dir=""
        record_skipped "оба тира сосуществуют на pod-cgroup" "idle=1 burst=quota"
    fi

    # 5. Окно наблюдения: минимум один полный цикл UpdateCgroups плюс событие пода.
    log "окно наблюдения ${SETTLE_SECONDS} с (цикл UpdateCgroups у kubelet — 60 с)"
    if kubectl_usable; then
        sleep 10
        log "создаю соседний под, чтобы заставить kubelet переписать QoS-слайс"
        neighbour_rc=0
        create_neighbour || neighbour_rc=$?
        record "соседний под форсирует перезапись слайса" \
            "создан" \
            "$([ "$neighbour_rc" -eq 0 ] && echo "создан" || echo "kubectl вернул ${neighbour_rc}")"
        sleep $(( SETTLE_SECONDS - 10 ))
    else
        record_skipped "соседний под форсирует перезапись слайса" "создан"
        log "kubectl недоступен: соседний под не создаётся, окно проверяет только цикл UpdateCgroups"
        sleep "$SETTLE_SECONDS"
    fi
    record "cpu.idle пережил цикл UpdateCgroups" "1" "$(read_knob "$idle_dir" cpu.idle)"
    if [ -n "$burst_dir" ]; then
        record "оба тира пережили цикл UpdateCgroups" \
            "idle=1 burst=${quota}" \
            "idle=$(read_knob "$burst_dir" cpu.idle) burst=$(read_knob "$burst_dir" cpu.max.burst)"
    fi

    # 6. Рестарт kubelet.
    if [ "$RESTART_KUBELET" -eq 1 ]; then
        log "перезапускаю kubelet"
        sudo systemctl restart kubelet
        sleep 25
        record "cpu.idle пережил restart kubelet" "1" "$(read_knob "$idle_dir" cpu.idle)"
        if [ -n "$burst_dir" ]; then
            record "оба тира пережили restart kubelet" \
                "idle=1 burst=${quota}" \
                "idle=$(read_knob "$burst_dir" cpu.idle) burst=$(read_knob "$burst_dir" cpu.max.burst)"
        fi
    else
        record_skipped "cpu.idle пережил restart kubelet" "1"
    fi

    # 7. Обратный переход теряет request-derived вес — ради этого агент его и восстанавливает.
    write_knob "$idle_dir" cpu.idle 0 || die "не удалось снять cpu.idle"
    got_weight="$(read_knob "$idle_dir" cpu.weight)"
    record "idle 1->0 оставляет вес по умолчанию, не ${want_weight}" \
        "$kernel_default_weight" "$got_weight"
}

# --- сухой прогон ------------------------------------------------------------

dry_run() {
    local want_weight
    want_weight="$(expected_weight 500)"
    cat <<EOF
Сухой прогон: ничего не создаётся и не пишется.

Namespace           : ${NAMESPACE}
Нода                : ${NODE_NAME}
Корень cgroup       : ${CGROUP_ROOT}
Минимальное ядро    : ${MIN_KERNEL}
Окно наблюдения     : ${SETTLE_SECONDS} с
Рестарт kubelet     : $([ "$RESTART_KUBELET" -eq 1 ] && echo включён || echo выключен)

Планируемые проверки и ожидаемые значения:
  1. путь pod-cgroup и вес из requests 500m        -> idle=0 weight=${want_weight}
  2. cpu.idle=1 сбрасывает вес в 1                 -> idle=1 weight=1
  3. запись cpu.weight при cpu.idle=1 отвергнута   -> запись отвергнута
  4. оба тира сосуществуют на pod-cgroup           -> idle=1 burst=quota
  5. cpu.idle пережил цикл UpdateCgroups           -> 1
  6. cpu.idle пережил restart kubelet              -> 1
  7. idle 1->0 оставляет вес по умолчанию          -> 100 (а не ${want_weight})

Ненулевой код возврата означает расхождение хотя бы в одной строке.
EOF
}

# --- main --------------------------------------------------------------------

main() {
    if [ "$DRY_RUN" -eq 1 ]; then
        dry_run
        exit 0
    fi

    preflight
    trap cleanup EXIT INT TERM

    if [ -z "$IDLE_POD_UID" ]; then
        create_pods
    else
        log "режим измерения: поды не создаются, namespace не трогается"
    fi

    run_checks
    print_table

    if [ "$FAILED" -ne 0 ]; then
        printf '\nРЕЗУЛЬТАТ: расхождение с эталоном. Смотрите строки со статусом ПРОВАЛ.\n'
        exit 1
    fi
    printf '\nРЕЗУЛЬТАТ: все проверки сошлись с эталоном, снятым на стенде.\n'
}

main
