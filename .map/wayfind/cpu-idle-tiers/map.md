<!-- DO NOT EDIT — regenerated from state.json by wayfind_runner.py. Manual edits will be overwritten. -->
# Wayfinding Map: Кодирование ортогональных CPU-тиров

- **Slug:** `cpu-idle-tiers`
- **Status:** handed_off
- **Map ID:** `9650164d080b41e18767d414de93d36a`
- **Revision:** 5

## Destination

Поправка к замороженной карте cpu-idle-operator: idle и burst — ортогональные переключатели, а не значения одного тира. Определить кодирование в аннотациях и уточнить, какие соседние knob'ы каждый из них реально блокирует.

## Notes

_None._

## Decisions so far

- **T-001** Кодирование двух независимых тиров — Два независимых ключа: cpu.azalio.net/tier: idle (порядок выбора) и cpu.azalio.net/burst (ширина полосы), под может нести оба — измерено, что idle, cpu.max и cpu.max.burst сосуществуют на одной cgroup без конфликта; поправка к прежнему выводу: idle запирает только cpu.weight, burst — только понижение quota, крест-накрест блокировок нет  ([resolution](resolutions/T-001.md))

## Frontier (resolve next, one non-research at a time)

_Empty._

## Blocked / claimed

_None._

## Fog of war (too vague to ticket yet)

_None._

## Out of scope

_None._
