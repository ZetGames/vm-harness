# vmh

Один бинарь на Go, через который агенты (и люди) создают и гоняют виртуалки
в VirtualBox и VMware Workstation без ручной возни: CLI с JSON-выводом,
HTTP API и MCP-сервер поверх одной библиотеки.

Обёртки над VBoxManage на Go существуют, но им по семь лет, они покрывают
половину команд и ничего не знают про VMware. Здесь оба гипервизора за одним
интерфейсом, а сверху — харнесс: агент не может сломать то, что создавал не он.

## Что умеет

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

## Харнесс

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

## Установка

```bash
go install github.com/fl4metf/vm-harness/cmd/vmh@latest
```

Нужен VirtualBox 7.x и/или VMware Workstation 17 / Fusion 13. Бинари ищутся
в PATH, в реестре Windows и в стандартных путях; можно указать в конфиге.

```bash
vmh providers
```

## Быстрый старт

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

## Зависшая загрузка

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

## VirtualBox поверх Hyper-V

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

## HTTP API

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

## MCP

```json
{ "mcpServers": { "vmh": { "command": "vmh", "args": ["mcp"] } } }
```

Тулзы: `vm_providers`, `vm_list`, `vm_get`, `vm_create`, `vm_power`,
`vm_delete`, `vm_update`, `vm_clone`, `vm_snapshot`, `vm_exec`, `vm_copy`,
`vm_write_file`, `vm_read_file`, `vm_ip`, `vm_wait`, `vm_screenshot`, `vm_port`.

## Как библиотека

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

## Конфиг

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

## Ограничения

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

## Тесты

```bash
go test ./...
VMH_INTEGRATION=1 go test -p 1 ./...
```

Интеграционные тесты создают настоящие ВМ `vmh-it-*` во временном каталоге
и удаляют их за собой.
