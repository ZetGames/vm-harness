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
- ожидание состояния: `running`, `stopped`, `ip`, `ssh`, `guest`.

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
  на файлы хоста.
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

`host_dirs` пустой — CLI и MCP работают с любыми путями пользователя, `serve`
ограничивается `<root>/files`. `allow_unmanaged: true` (или
`VMH_ALLOW_UNMANAGED=1`) снимает защиту чужих ВМ целиком — это для людей.

Переменные: `VMH_ROOT`, `VMH_CONFIG`, `VMH_PROVIDER`, `VMH_VBOXMANAGE`,
`VMH_VMRUN`, `VMH_MAX_VMS`, `VMH_OUTPUT`, `VMH_TOKEN`.

## Ограничения

- У VMware нет проброса портов на уровне ВМ: SSH ходит прямо на IP гостя,
  а IP отдают VMware Tools (в cloud-образах Ubuntu они есть).
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
