# vmh

English · [Русский](#русский)

A single Go binary that lets agents (and people) create and run virtual machines
on VirtualBox and VMware Workstation without the manual fiddling: a CLI with JSON
output, an HTTP API and an MCP server, all on top of one library.

Go wrappers around VBoxManage do exist, but they are seven years old, cover half
of the commands and know nothing about VMware. vmh puts both hypervisors behind
one interface and adds a harness on top: an agent cannot break what it did not create.

## Features

- create a VM from an ISO, from a copy of a disk image (vmdk/vdi/vhd), from an OVA,
  or with a blank disk;
- cloud-init out of the box: vmh builds the seed ISO, generates an SSH key and
  forwards a port to 22, so `exec` works as soon as `vmh wait --for ssh` returns;
- unattended OS installation from an ISO (VirtualBox);
- start/stop (graceful, falling back to a hard power-off after a timeout), pause,
  reset, suspend;
- snapshots, full and linked clones (a clone of a cloud-init VM gets a new
  instance-id, so its network comes up with the new MAC);
- exec and file copy over SSH or through guest tools
  (VirtualBox Guest Additions, VMware Tools);
- guest IP, screenshots, NAT port forwarding (VirtualBox), shared folders;
- waiting for `running`, `stopped`, `paused`, `saved`, `ip`, `ssh`, `guest`;
  vmh resets a stuck boot by itself while it waits.

## The harness

- Managed VMs are the ones vmh created: they live under its root or carry a
  marker equal to their own ID. A copy or an import of someone else's machine
  loses the marker. Only managed VMs can be changed, started, deleted, cloned or
  exec'd into. Others show up in `vmh ls`, but vmh won't touch them until a person
  runs `vmh adopt` in an interactive terminal. Adopt is not available over HTTP or MCP.
- A linked clone keeps its base from being deleted.
- Limits on the number of VMs, CPUs, memory and disk. Limits and locks work across
  processes: several agents each running their own `vmh mcp` can't race each other.
- Host files: writes into other VMs' folders, into hypervisor files and inside the
  vmh root (except `<root>/files`) are refused. Paths like `\\?\`, `\\localhost\C$`,
  NTFS alternate data streams and junctions are canonicalised or rejected.
- An OVA is treated as untrusted: after the import vmh removes extradata, serial
  ports, shared folders, NIC tracing and other settings that point at host files.
  vmh adds its own cloud-init `serial.log` only after that clean-up.
- Passwords never go on the VBoxManage command line; keys, seeds with passwords and
  the API token are readable only by the current user (through ACLs on Windows).
- Errors look the same everywhere: a stable code (`not_found`, `forbidden`,
  `invalid_state`, ...) in JSON and a matching exit code.

## Installation

```bash
go install github.com/ZetGames/vm-harness/cmd/vmh@latest
```

Requires VirtualBox 7.x and/or VMware Workstation 17 / Fusion 13. The hypervisor
binaries are found in PATH, in the Windows registry and in the standard install
paths; they can also be set in the config.

```bash
vmh providers
```

## Quick start

An Ubuntu cloud image with SSH ready to go:

```bash
vmh create web --image noble-server-cloudimg-amd64.vmdk --os ubuntu --cloud-init --start
vmh wait web --for ssh --timeout 5m
vmh exec web -- uname -a
vmh cp ./app.tar web:/tmp/
vmh snap take web clean
vmh clone web web-2 --linked
vmh rm --force web-2
```

Windows from scratch:

```bash
vmh create win11 --iso Win11.iso --os windows11 --unattended --user admin --password Secret1 --start
```

When stdout is not a terminal, vmh prints exactly one JSON document per command,
errors included: `{"error":{"code":"...","message":"..."}}`. `vmh exec` exits with
the guest command's exit code (125 if vmh itself failed). All commands: `vmh --help`.

## Stuck boots

VMs that vmh creates with cloud-init write the guest's serial console to
`<VM folder>/serial.log` (on VirtualBox and VMware; a clone writes to its own
folder). The path is in the `console_log` field of `vmh show` (`console` in text
mode), `GET /v1/vms/{vm}` and `vm_get`. The Ubuntu cloud image logs its whole boot
there: kernel, initramfs, systemd, cloud-init and finally `<hostname> login:`.

While `vmh wait` (and `vmh ip --wait`, `vm_wait`, `POST /v1/vms/{vm}/wait`) waits
for `ip`, `ssh` or `guest` and the check keeps failing, vmh reads that log. Only the
current boot counts: the output after the last `Linux version` line and after the
last successful start or reset done through vmh. VirtualBox keeps appending to the
log across resets, VMware recreates it at power-on; vmh handles both. A hard reset
happens when the current boot:

- contains `Kernel panic` — right away, even if the panic happened before the wait
  started;
- contains `Invalid MAC Address`: the NIC came up broken (this happens to e1000
  after a reset in the middle of a boot), so there will be no network in this boot;
- has not written to the log for 90 seconds while the boot reached neither `login:`
  nor the `Cloud-init v. ... finished` line. The clock runs from the last write to
  the file, not from the start of the wait, so short repeated calls (`vm_wait` with a
  small `timeout_sec`) also get to the reset.

The reset runs under the VM lock and after the log is read again: if someone else
reset the VM in the meantime (another wait, `vmh reset`, another vmh process) or the
boot moved on, there is no second reset. While the VM is paused, saved or busy with
another vmh operation (a snapshot, for example) it is left alone, and that time does
not count toward the 90 seconds. At most two resets per wait; if the boot is stuck
again after the second one, the wait ends right away with `not_ready` (exit 10), the
cause and the list of resets, instead of waiting out `--timeout`. The next wait may
reset the VM up to two times again, so fix the cause first (the error says where to
look: the console log or the vCPU count). Every reset, including one the hypervisor
answered with an error, is listed in the result's `recoveries`
(`kernel panic: IO-APIC + timer doesn't work! — reset`,
`boot stalled for 90s at: Begin: Loading essential drivers ... — reset`), printed to
stderr in text mode, and mentioned by any wait error that follows it:
`... after 2 automatic resets (...)`. If a reset returned an error, vmh treats the boot
as unchanged: a panic or a broken MAC is reset again on the next check, a stall after
another 90 seconds, within the same two resets.

Only VMs that vmh manages are reset: the ones it created or that were taken over
with `vmh adopt`. Others never are, not even with `allow_unmanaged`; Windows guests
never are either. Without a `console_log` vmh just waits.

## VirtualBox on top of Hyper-V

When the Windows hypervisor is running (it is started by the Hyper-V feature,
Memory integrity (VBS), WSL2, Docker Desktop, Windows Sandbox and Virtual Machine
Platform), VirtualBox runs guests through the Windows Hypervisor Platform instead of
AMD-V/VT-x. Guests boot slowly, and with several vCPUs unreliably. Measured on such a
host (VirtualBox 7.2, Ubuntu 24.04 cloud image, virtio-net, 4 cold boots per variant):

| vCPU | paravirtualization | booted | how it fails |
|---|---|---|---|
| 1 | default (KVM) | 4/4, in 27–32 s | — |
| 1 | hyperv or none | 0/8 | `Kernel panic - not syncing: IO-APIC + timer doesn't work!` |
| 2 | default (KVM) | 3/4 | hangs at `Begin: Loading essential drivers ...` |
| 2 | hyperv | 2/4 | hangs in the initramfs |
| 2 | none | 3/4 | hangs in systemd |

So vmh:

- detects Hyper-V through CPUID (the hypervisor bit, the `Microsoft Hv` vendor and
  the root partition privileges) and prints a warning in `vmh providers` (the
  `warnings` field in JSON and in `vm_providers`). If the host itself is a VM under
  some other hypervisor, the warning talks about nested virtualization instead;
- reports `max_reliable_cpus: 1` for VirtualBox on Hyper-V, and `vmh create` without
  `--cpus` (without `cpus` in the API) creates the VM with 1 vCPU even if
  `defaults.cpus` in the config or the imported OVA asks for more. An explicit vCPU
  count is kept, and a clone gets as many as its source;
- leaves VirtualBox's paravirtualization at its default;
- gives the first NIC of Linux VMs with cloud-init the `virtio` model: after a reset in
  the middle of a boot, e1000 came up with a broken EEPROM and a zero MAC in 3 cases
  out of 4. Windows VMs and VMs without cloud-init get VirtualBox's default model, and a
  model set in the spec (`nics[].model`) is left alone;
- resets stuck boots in `vmh wait` for managed non-Windows VMs with a `console_log`
  (see above); with 2 vCPUs that usually saves the day.

To get VirtualBox back on AMD-V/VT-x, fast and stable, turn the Windows hypervisor
off: everything listed above, or `bcdedit /set hypervisorlaunchtype off` as
administrator and a reboot (WSL2 and Docker Desktop won't start without it;
`hypervisorlaunchtype auto` brings it back).

## HTTP API

```bash
vmh serve
VMH_TOKEN=secret vmh serve --addr 0.0.0.0:8070
```

A token is always required: if none is given, vmh generates a new one and writes it
to `<root>/serve.token`. Requests with a foreign `Host`, with an `Origin` header, or
with JSON sent without `Content-Type: application/json` are refused, so a browser
can't drive the API. Host paths in requests are limited to `<root>/files` and the
`host_dirs` from the config.

Routes under `/v1`: `GET/POST /vms`, `GET/PATCH/DELETE /vms/{vm}`,
`POST /vms/{vm}/{start,stop,pause,resume,reset,suspend,clone,exec,wait}`,
`/vms/{vm}/snapshots`, `/vms/{vm}/ports`, `/vms/{vm}/files?path=`,
`GET /vms/{vm}/ip`, `GET /vms/{vm}/screenshot`, `GET /providers`.

## MCP

```json
{ "mcpServers": { "vmh": { "command": "vmh", "args": ["mcp"] } } }
```

Tools: `vm_providers`, `vm_list`, `vm_get`, `vm_create`, `vm_power`,
`vm_delete`, `vm_update`, `vm_clone`, `vm_snapshot`, `vm_exec`, `vm_copy`,
`vm_write_file`, `vm_read_file`, `vm_ip`, `vm_wait`, `vm_screenshot`, `vm_port`.

## As a library

```go
cfg, err := harness.LoadConfig("")
if err != nil {
	return err
}
m := harness.Open(cfg)
machine, err := m.Create(ctx, vm.Spec{
	Name:      "web",
	OSType:    "ubuntu",
	DiskImage: "/images/noble.vmdk",
	CloudInit: &vm.CloudInit{},
	Start:     true,
})
```

The providers can be used directly too: `virtualbox.New` and `vmware.New`
implement `vm.Provider`, but then without the harness.

## Configuration

`~/.vmh/config.json` (the root can be changed with `VMH_ROOT`):

```json
{
  "default_provider": "virtualbox",
  "host_dirs": ["D:\\images", "D:\\work"],
  "limits": { "max_vms": 10, "max_cpus": 8, "max_memory_mb": 16384, "max_disk_gb": 200 },
  "defaults": { "cpus": 2, "memory_mb": 2048, "disk_gb": 20, "os_type": "ubuntu" },
  "vmware": { "vmrun": "D:\\vmware\\vmrun.exe" }
}
```

If a provider reports `max_reliable_cpus` (VirtualBox on Hyper-V: 1, see above),
`defaults.cpus` is capped to it, and an OVA import without `cpus` gets exactly that
many vCPUs.

With an empty `host_dirs` the CLI and MCP work with any of the user's paths, while
`serve` is limited to `<root>/files`. `allow_unmanaged: true` (or
`VMH_ALLOW_UNMANAGED=1`) lifts the protection of other VMs entirely; that is meant
for people.

Environment variables: `VMH_ROOT`, `VMH_CONFIG`, `VMH_PROVIDER`, `VMH_VBOXMANAGE`,
`VMH_VMRUN`, `VMH_MAX_VMS`, `VMH_OUTPUT`, `VMH_TOKEN`.

## Limitations

- VMware has no per-VM port forwarding: SSH goes straight to the guest IP. The IP
  comes from VMware Tools (Ubuntu cloud images include them). If vmrun thinks Tools
  are not running (a known bug with headless VMs), vmh takes the address from the
  VMware DHCP lease of the VM's MAC: `%ProgramData%\VMware\vmnetdhcp.leases` on
  Windows, `/etc/vmware/vmnet*/dhcpd/dhcpd.leases` on Linux,
  `/var/db/vmware/*.leases` on macOS (best effort there: Fusion with vmnet.framework
  networking may not write such files). Only a lease that has not expired and started
  no earlier than the VM's current power-on (the first timestamp in
  `<VM folder>/vmware.log`, with 5 seconds of slack) is used: vmh won't return an
  address the guest got before the VM was powered off, and until the guest gets a new
  one the answer stays `not_ready`. A reset (both `vmh reset` and the automatic one in
  `vmh wait`) and a reboot inside the guest don't count as a new power-on, because the
  VMware process stays the same, so until Tools answer vmh may return the address from
  a lease taken before the reset. A bridged network has no such lease.
- vmrun's `runProgramInGuest` doesn't return output, so exec through VMware Tools
  writes stdout/stderr to temporary files in the guest and copies them back.
- VMware can't clone a running machine or a snapshot taken while it was running.
- Unattended installation is VirtualBox only.

## Tests

```bash
go test ./...
VMH_INTEGRATION=1 go test -p 1 ./...
```

Integration tests create real VMs named `vmh-it-*` in a temporary directory and
delete them afterwards.

## License

MIT, see [LICENSE](LICENSE).

---

## Русский

[English](#vmh) · Русский

Один бинарь на Go, через который агенты (и люди) создают и гоняют виртуалки
в VirtualBox и VMware Workstation без ручной возни: CLI с JSON-выводом,
HTTP API и MCP-сервер поверх одной библиотеки.

Обёртки над VBoxManage на Go существуют, но им по семь лет, они покрывают
половину команд и ничего не знают про VMware. Здесь оба гипервизора за одним
интерфейсом, а сверху — харнесс: агент не может сломать то, что создавал не он.

### Что умеет

- создать ВМ из ISO, из копии образа диска (vmdk/vdi/vhd), из OVA или с пустым диском;
- cloud-init из коробки: vmh сам собирает seed ISO, генерирует SSH-ключ и
  пробрасывает порт до 22 — после `vmh wait --for ssh` сразу работает `exec`;
- unattended-установку ОС с ISO (VirtualBox);
- старт/стоп (мягкий, с фолбэком на жёсткий по таймауту), пауза, ресет, suspend;
- снапшоты, полные и связанные клоны (клон cloud-init машины получает новый
  instance-id, поэтому сеть поднимается с новым MAC);
- exec и копирование файлов через SSH или через guest tools
  (VirtualBox Guest Additions, VMware Tools);
- IP гостя, скриншот, проброс портов NAT (VirtualBox), shared folders;
- ожидание: `running`, `stopped`, `paused`, `saved`, `ip`, `ssh`, `guest`;
  зависшую загрузку vmh по ходу ожидания ресетит сам.

### Харнесс

- Managed — это машины, которые создал vmh: они лежат в его корне или несут
  метку, равную их собственному ID. Копия или импорт чужой машины метку теряет.
  Менять, запускать, удалять, клонировать и exec-ать можно только managed.
  Остальные видно в `vmh ls`, но трогать их vmh не даст, пока человек не сделает
  `vmh adopt` в интерактивном терминале. Через HTTP и MCP adopt недоступен.
- Связанный клон не даст удалить свою базу.
- Лимиты на число ВМ, CPU, память и диск. Лимиты и блокировки работают между
  процессами: несколько агентов с собственными `vmh mcp` не обгонят друг друга.
- Файлы хоста: запись в папки чужих ВМ, в файлы гипервизора и внутрь корня vmh
  (кроме `<root>/files`) запрещена. Пути вида `\\?\`, `\\localhost\C$`,
  альтернативные потоки NTFS и junction'ы канонизируются или отклоняются.
- OVA считается недоверенным: после импорта vmh вычищает extradata, последовательные
  порты, shared folders, трассировку NIC и прочие настройки, которые указывают
  на файлы хоста. Свой `serial.log` для cloud-init vmh добавляет уже после чистки.
- Пароли не попадают в командную строку VBoxManage; ключи, seed с паролем и токен
  доступны только текущему пользователю (на Windows — через ACL).
- Ошибки везде одинаковые: стабильный код (`not_found`, `forbidden`,
  `invalid_state`, ...) в JSON и соответствующий exit code.

### Установка

```bash
go install github.com/ZetGames/vm-harness/cmd/vmh@latest
```

Нужен VirtualBox 7.x и/или VMware Workstation 17 / Fusion 13. Бинари ищутся
в PATH, в реестре Windows и в стандартных путях; можно указать в конфиге.

```bash
vmh providers
```

### Быстрый старт

Ubuntu cloud image с готовым SSH:

```bash
vmh create web --image noble-server-cloudimg-amd64.vmdk --os ubuntu --cloud-init --start
vmh wait web --for ssh --timeout 5m
vmh exec web -- uname -a
vmh cp ./app.tar web:/tmp/
vmh snap take web clean
vmh clone web web-2 --linked
vmh rm --force web-2
```

Windows с нуля:

```bash
vmh create win11 --iso Win11.iso --os windows11 --unattended --user admin --password Secret1 --start
```

Когда stdout не терминал, vmh печатает ровно один JSON-документ на команду,
ошибки тоже: `{"error":{"code":"...","message":"..."}}`. `vmh exec` выходит
с кодом команды в госте (125 — если упал сам vmh). Все команды — `vmh --help`.

### Зависшая загрузка

ВМ, которые vmh создаёт с cloud-init, пишут последовательную консоль гостя
в `<папка ВМ>/serial.log` (и VirtualBox, и VMware, клон пишет в свою папку).
Путь лежит в поле `console_log` у `vmh show` (в тексте — `console`),
`GET /v1/vms/{vm}` и `vm_get`. Ubuntu cloud image выводит туда всю загрузку:
ядро, initramfs, systemd, cloud-init и в конце `<hostname> login:`.

Пока `vmh wait` (и `vmh ip --wait`, `vm_wait`, `POST /v1/vms/{vm}/wait`) ждёт
`ip`, `ssh` или `guest`, а проверка не проходит, vmh смотрит в этот лог.
В счёт идёт только текущая загрузка: вывод после последней строки
`Linux version` и после последнего удавшегося старта или ресета через vmh.
VirtualBox дописывает лог через ресеты, VMware пересоздаёт его при включении —
vmh различает оба случая. Жёсткий ресет делается, если в текущей загрузке:

- есть `Kernel panic` — сразу, даже если паника случилась до начала ожидания;
- есть `Invalid MAC Address`: сетевая карта поднялась сломанной (так бывает
  с e1000 после ресета посреди загрузки), сети в этой загрузке не будет;
- лог не пишется 90 секунд, а загрузка не дошла ни до `login:`, ни до строки
  `Cloud-init v. ... finished`. Отсчёт идёт от последней записи в файл, а не
  от начала ожидания, поэтому короткие повторные вызовы (`vm_wait` с маленьким
  `timeout_sec`) тоже доходят до ресета.

Ресет идёт под блокировкой ВМ и после повторного чтения лога: если ВМ за это
время ресетнул кто-то ещё (другое ожидание, `vmh reset`, другой процесс vmh)
или загрузка сдвинулась, второго ресета не будет. Пока ВМ на паузе, сохранена
или занята другой операцией vmh (например, снапшотом), её не трогают, и это
время в 90 секунд не входит. Не больше двух ресетов за одно ожидание; если
после второго загрузка снова зависла, ожидание сразу заканчивается ошибкой
`not_ready` (exit 10) с причиной и списком ресетов, а не ждёт `--timeout`.
Следующее ожидание снова может ресетнуть ВМ до двух раз, поэтому сначала надо
устранить причину (ошибка подсказывает, где её искать: лог консоли или число
vCPU). Каждый ресет — и тот, на который гипервизор ответил ошибкой, — попадает
в `recoveries` результата (`kernel panic: IO-APIC + timer doesn't work! — reset`,
`boot stalled for 90s at: Begin: Loading essential drivers ... — reset`),
в текстовом режиме печатается в stderr, а любая ошибка ожидания после ресета
его упоминает: `... after 2 automatic resets (...)`. Если ресет вернул ошибку,
vmh считает загрузку прежней: паника или битый MAC ресетятся снова на следующей
проверке, зависание — ещё через 90 секунд, в пределах тех же двух ресетов.

Ресетятся только ВМ, которыми управляет vmh: созданные им или взятые через
`vmh adopt`. Остальные — никогда, даже с `allow_unmanaged`; Windows-гости
тоже никогда. Без `console_log` vmh просто ждёт.

### VirtualBox поверх Hyper-V

Если в Windows работает гипервизор (его поднимают компонент Hyper-V,
«Изоляция ядра» (VBS), WSL2, Docker Desktop, Windows Sandbox и Virtual Machine
Platform), VirtualBox запускает гостей не на AMD-V/VT-x, а через Windows
Hypervisor Platform. Гости грузятся медленно, а с несколькими vCPU — ненадёжно.
Замер на таком хосте (VirtualBox 7.2, Ubuntu 24.04 cloud image, virtio-net,
по 4 холодные загрузки на вариант):

| vCPU | паравиртуализация | загрузилось | как ломается |
|---|---|---|---|
| 1 | default (KVM) | 4/4, за 27–32 с | — |
| 1 | hyperv или none | 0/8 | `Kernel panic - not syncing: IO-APIC + timer doesn't work!` |
| 2 | default (KVM) | 3/4 | висит на `Begin: Loading essential drivers ...` |
| 2 | hyperv | 2/4 | висит в initramfs |
| 2 | none | 3/4 | висит в systemd |

Поэтому vmh:

- узнаёт Hyper-V по CPUID (бит гипервизора, вендор `Microsoft Hv` и права
  корневого раздела) и пишет предупреждение в `vmh providers` (поле `warnings`
  в JSON и в `vm_providers`). Если сам хост — виртуалка в чужом гипервизоре,
  предупреждение говорит про вложенную виртуализацию, а не про Hyper-V;
- на Hyper-V отдаёт у VirtualBox `max_reliable_cpus: 1`, и `vmh create` без
  `--cpus` (без `cpus` в API) создаёт ВМ с 1 vCPU, даже если `defaults.cpus`
  в конфиге больше или в импортируемом OVA записано больше. Явное число vCPU
  не меняется, клон получает столько же, сколько у исходной ВМ;
- оставляет паравиртуализацию VirtualBox по умолчанию;
- ставит первой сетевой карте Linux-ВМ с cloud-init модель `virtio`: e1000
  после ресета посреди загрузки в 3 случаях из 4 поднималась с битой EEPROM
  и нулевым MAC. Windows-ВМ и ВМ без cloud-init получают модель VirtualBox по
  умолчанию, модель из spec (`nics[].model`) не меняется;
- ресетит зависшие загрузки в `vmh wait` у управляемых не-Windows ВМ
  с `console_log` (см. выше) — на 2 vCPU это обычно выручает.

Чтобы VirtualBox снова работал на AMD-V/VT-x быстро и стабильно, гипервизор
Windows выключают: всё перечисленное выше или `bcdedit /set hypervisorlaunchtype off`
от администратора и перезагрузка (WSL2 и Docker Desktop без него не запустятся;
вернуть — `hypervisorlaunchtype auto`).

### HTTP API

```bash
vmh serve
VMH_TOKEN=secret vmh serve --addr 0.0.0.0:8070
```

Токен нужен всегда: если его не задать, vmh сгенерирует новый и положит
в `<root>/serve.token`. Запросы с чужим `Host`, с `Origin` и JSON без
`Content-Type: application/json` отклоняются — браузер не сможет дёрнуть API.
Пути хоста в запросах ограничены `<root>/files` и `host_dirs` из конфига.

Маршруты под `/v1`: `GET/POST /vms`, `GET/PATCH/DELETE /vms/{vm}`,
`POST /vms/{vm}/{start,stop,pause,resume,reset,suspend,clone,exec,wait}`,
`/vms/{vm}/snapshots`, `/vms/{vm}/ports`, `/vms/{vm}/files?path=`,
`GET /vms/{vm}/ip`, `GET /vms/{vm}/screenshot`, `GET /providers`.

### MCP

```json
{ "mcpServers": { "vmh": { "command": "vmh", "args": ["mcp"] } } }
```

Тулзы: `vm_providers`, `vm_list`, `vm_get`, `vm_create`, `vm_power`,
`vm_delete`, `vm_update`, `vm_clone`, `vm_snapshot`, `vm_exec`, `vm_copy`,
`vm_write_file`, `vm_read_file`, `vm_ip`, `vm_wait`, `vm_screenshot`, `vm_port`.

### Как библиотека

```go
cfg, err := harness.LoadConfig("")
if err != nil {
	return err
}
m := harness.Open(cfg)
machine, err := m.Create(ctx, vm.Spec{
	Name:      "web",
	OSType:    "ubuntu",
	DiskImage: "/images/noble.vmdk",
	CloudInit: &vm.CloudInit{},
	Start:     true,
})
```

Провайдеры можно использовать и напрямую: `virtualbox.New` и `vmware.New`
реализуют `vm.Provider`, но тогда без харнесса.

### Конфиг

`~/.vmh/config.json` (корень меняется через `VMH_ROOT`):

```json
{
  "default_provider": "virtualbox",
  "host_dirs": ["D:\\images", "D:\\work"],
  "limits": { "max_vms": 10, "max_cpus": 8, "max_memory_mb": 16384, "max_disk_gb": 200 },
  "defaults": { "cpus": 2, "memory_mb": 2048, "disk_gb": 20, "os_type": "ubuntu" },
  "vmware": { "vmrun": "D:\\vmware\\vmrun.exe" }
}
```

Если провайдер сообщает `max_reliable_cpus` (VirtualBox на Hyper-V: 1, см. выше),
`defaults.cpus` урезается до него, а импорт OVA без `cpus` получает ровно
столько vCPU.

`host_dirs` пустой — CLI и MCP работают с любыми путями пользователя, `serve`
ограничивается `<root>/files`. `allow_unmanaged: true` (или
`VMH_ALLOW_UNMANAGED=1`) снимает защиту чужих ВМ целиком — это для людей.

Переменные: `VMH_ROOT`, `VMH_CONFIG`, `VMH_PROVIDER`, `VMH_VBOXMANAGE`,
`VMH_VMRUN`, `VMH_MAX_VMS`, `VMH_OUTPUT`, `VMH_TOKEN`.

### Ограничения

- У VMware нет проброса портов на уровне ВМ: SSH ходит прямо на IP гостя.
  IP отдают VMware Tools (в cloud-образах Ubuntu они есть). Если vmrun считает,
  что Tools не запущены (известный баг с headless-машинами), vmh берёт адрес
  из DHCP-аренды VMware по MAC машины: `%ProgramData%\VMware\vmnetdhcp.leases`
  на Windows, `/etc/vmware/vmnet*/dhcpd/dhcpd.leases` на Linux,
  `/var/db/vmware/*.leases` на macOS (там это best effort: Fusion с сетью через
  vmnet.framework таких файлов может не писать). Годится только аренда, которая
  ещё не истекла и началась не раньше текущего включения ВМ (первая метка
  времени в `<папка ВМ>/vmware.log`, допуск 5 секунд): адрес, полученный до
  выключения ВМ, vmh не вернёт, пока гость не получит новый, и ответ остаётся
  `not_ready`. Ресет (и `vmh reset`, и автоматический в `vmh wait`) и
  перезагрузка изнутри гостя новым включением не считаются: процесс VMware тот
  же, поэтому, пока Tools не ответят, vmh может отдать адрес из аренды,
  полученной до ресета. У bridged-сети такой аренды нет.
- `runProgramInGuest` у vmrun не отдаёт вывод, поэтому exec через VMware Tools
  пишет stdout/stderr во временные файлы в госте и забирает их.
- VMware не умеет клонировать работающую машину и снапшот, снятый на ходу.
- Unattended-установка — только VirtualBox.

### Тесты

```bash
go test ./...
VMH_INTEGRATION=1 go test -p 1 ./...
```

Интеграционные тесты создают настоящие ВМ `vmh-it-*` во временном каталоге
и удаляют их за собой.

### Лицензия

MIT, текст в [LICENSE](LICENSE).
