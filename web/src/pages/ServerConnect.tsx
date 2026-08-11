import { useCallback, useEffect, useState } from "react";

import * as api from "../lib/api";

interface Props {
  serverId: string;
}

/** Как называется сеть, из которой придёт друг. */
const KIND_LABELS: Record<api.AddressKind, string> = {
  public: "Из интернета",
  hamachi: "Hamachi",
  radmin: "Radmin VPN",
  zerotier: "ZeroTier",
  tailscale: "Tailscale",
  lan: "Локальная сеть",
  virtual: "Виртуальный адаптер",
  reserved: "Служебный адрес",
  loopback: "Только эта машина",
};

/** Кому этот адрес пригодится. Без пояснения список из четырёх строк — загадка. */
const KIND_HINTS: Record<api.AddressKind, string> = {
  public: "работает у всех",
  hamachi: "для тех, кто вошёл в вашу сеть Hamachi",
  radmin: "для тех, кто вошёл в вашу сеть Radmin VPN",
  zerotier: "для тех, кто в вашей сети ZeroTier",
  tailscale: "для тех, кто в вашей сети Tailscale",
  lan: "для тех, кто в той же квартире или офисе",
  virtual: "адаптер виртуалки или контейнера — снаружи туда не попасть",
  reserved:
    "служебный диапазон, обычно адаптер VPN-клиента — снаружи не работает",
  loopback: "не годится никому, кроме вас",
};

/**
 * «Как подключиться» — ответ на вопрос, который панели на домашней машине
 * задают чаще всего.
 *
 * У такой машины четыре адреса, и ничто не подсказывает, какой из них рабочий.
 * Люди раздают друзьям localhost и потом вечер выясняют, почему никто не
 * заходит.
 */
export function ServerConnect({ serverId }: Props) {
  const [info, setInfo] = useState<api.ConnectInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      setInfo(await api.serverConnect(serverId));
      setError(null);
    } catch (err) {
      setError(
        err instanceof api.ApiError
          ? err.message
          : "Не удалось получить адреса",
      );
    }
  }, [serverId]);

  useEffect(() => {
    void reload();
  }, [reload]);

  async function copy(address: string) {
    try {
      await navigator.clipboard.writeText(address);
      setCopied(address);
      setTimeout(() => setCopied(null), 1500);
    } catch {
      // Буфер обмена может быть недоступен без https — адрес виден и так.
    }
  }

  async function forward() {
    setBusy(true);
    setError(null);
    try {
      setInfo(await api.forwardPort(serverId));
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Роутер не ответил");
    } finally {
      setBusy(false);
    }
  }

  async function enableRelay() {
    setBusy(true);
    setError(null);
    try {
      setInfo(await api.enableRelay(serverId));
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Релей не ответил");
    } finally {
      setBusy(false);
    }
  }

  async function disableRelay() {
    setBusy(true);
    setError(null);
    try {
      await api.disableRelay(serverId);
      await reload();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Релей не ответил");
    } finally {
      setBusy(false);
    }
  }

  async function unforward() {
    setBusy(true);
    setError(null);
    try {
      await api.unforwardPort(serverId);
      await reload();
    } catch (err) {
      setError(err instanceof api.ApiError ? err.message : "Роутер не ответил");
    } finally {
      setBusy(false);
    }
  }

  if (!info) {
    return (
      <section className="card p-4">
        <p className="text-sm text-muted">
          {error ?? "Смотрю, какие адреса есть у этой машины…"}
        </p>
      </section>
    );
  }

  return (
    <div className="grid gap-4">
      {error && (
        <p className="rounded-sm border border-danger bg-danger-bg px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      <section className="card overflow-hidden">
        <header className="border-b border-line px-4 py-2 text-sm text-muted">
          Адреса для друзей
        </header>

        <ul className="divide-y divide-line">
          {info.addresses.map((addr) => (
            <li
              key={addr.address}
              className="flex flex-wrap items-center gap-3 px-4 py-2.5"
            >
              <code className="font-mono text-sm">{addr.address}</code>
              <span className="text-sm text-muted">
                {KIND_LABELS[addr.kind]}
              </span>
              <span className="text-xs text-faint">
                {KIND_HINTS[addr.kind]}
              </span>
              {addr.interface && (
                <span className="text-xs text-faint">· {addr.interface}</span>
              )}
              <button
                type="button"
                className="ml-auto text-xs text-accent hover:underline"
                onClick={() => void copy(addr.address)}
              >
                {copied === addr.address ? "скопировано" : "копировать"}
              </button>
            </li>
          ))}
        </ul>
      </section>

      <Internet
        info={info}
        busy={busy}
        onForward={forward}
        onUnforward={unforward}
      />

      {info.relay.configured && (
        <Relay
          info={info}
          busy={busy}
          onEnable={enableRelay}
          onDisable={disableRelay}
        />
      )}
    </div>
  );
}

/**
 * Что с доступом из интернета — и, если его нет, почему именно.
 *
 * Причина важнее факта: «проброс не поможет, у провайдера серый адрес» и
 * «роутер умеет, надо нажать кнопку» — это два разных вечера.
 */
function Internet({
  info,
  busy,
  onForward,
  onUnforward,
}: {
  info: api.ConnectInfo;
  busy: boolean;
  onForward: () => void;
  onUnforward: () => void;
}) {
  const {
    state,
    external_ip: externalIP,
    address,
    taken_by: takenBy,
  } = info.internet;

  return (
    <section className="card p-4">
      <p className="mb-2 text-sm text-muted">Из интернета</p>

      {state === "direct" && (
        <p className="text-sm">
          У этой машины свой публичный адрес — друзьям хватит{" "}
          <code className="font-mono">{address}</code>. Роутер ни при чём.
        </p>
      )}

      {state === "forwarded" && (
        <div className="grid gap-2">
          <p className="text-sm">
            Роутер уже отправляет порт {info.port} на эту машину. Адрес для
            друзей: <code className="font-mono">{address}</code>
          </p>
          <button
            type="button"
            className="btn btn-ghost w-fit text-sm"
            disabled={busy}
            onClick={onUnforward}
          >
            Убрать проброс
          </button>
        </div>
      )}

      {state === "can_forward" && (
        <div className="grid gap-2">
          <p className="text-sm">
            Машина за роутером, и роутер умеет пробрасывать порты. Если
            попросить — друзьям не понадобится ни Hamachi, ни что-либо ещё
            {externalIP ? `, адрес будет ${externalIP}:${info.port}` : ""}.
          </p>
          <p className="text-xs text-faint">
            Это изменит настройки роутера — того самого, что раздаёт интернет
            всей квартире. Убрать можно здесь же.
          </p>
          <button
            type="button"
            className="btn btn-primary w-fit"
            disabled={busy}
            onClick={onForward}
          >
            {busy ? "Спрашиваю роутер…" : `Открыть порт ${info.port} наружу`}
          </button>
        </div>
      )}

      {state === "taken_by_another" && (
        <p className="text-sm">
          Порт {info.port} роутер уже отправляет на другую машину ({takenBy}).
          Панель его не отбирает: там может работать что-то нужное. Либо
          освободите порт на роутере вручную, либо смените порт сервера в
          параметрах.
        </p>
      )}

      {state === "carrier_nat" && (
        <div className="grid gap-1">
          <p className="text-sm">
            Провайдер выдал вашему роутеру не настоящий адрес, а общий с другими
            абонентами
            {externalIP ? ` (${externalIP})` : ""}. Проброс порта тут не поможет
            — сколько ни настраивай роутер.
          </p>
          <p className="text-sm text-muted">
            Рабочих пути два: попросить у провайдера белый IP (обычно платно)
            или собрать друзей в одной сети — Hamachi, Radmin VPN, ZeroTier.
            Тогда годится адрес из списка выше.
          </p>
        </div>
      )}

      {state === "no_router" && (
        <div className="grid gap-1">
          <p className="text-sm">
            Роутер не отвечает на автоматический запрос — обычно это значит, что
            UPnP в нём выключен. Это нормальная настройка, а не поломка.
          </p>
          <p className="text-sm text-muted">
            Пробросьте порт {info.port} вручную в настройках роутера или
            соберите друзей в одной сети через Hamachi или Radmin VPN — адрес из
            списка выше.
          </p>
        </div>
      )}
    </section>
  );
}

/**
 * Туннель через машину с белым адресом.
 *
 * Последний ответ на вопрос «а если провайдер выдал общий адрес»: тогда ни
 * проброс порта, ни настройка роутера не помогут никак, а друзьям всё равно
 * нужен обычный адрес, который вводят в игре.
 */
function Relay({
  info,
  busy,
  onEnable,
  onDisable,
}: {
  info: api.ConnectInfo;
  busy: boolean;
  onEnable: () => void;
  onDisable: () => void;
}) {
  const { enabled, address, held_by: heldBy, error } = info.relay;

  return (
    <section className="card p-4">
      <p className="mb-2 text-sm text-muted">Через релей</p>

      {!enabled && !heldBy && (
        <div className="grid gap-2">
          <p className="text-sm">
            У панели есть релей — машина с белым адресом, которая пропускает
            игроков к этому серверу. Панель подключается к ней сама, изнутри: на
            этом компьютере ничего открывать не нужно, а друзьям достанется
            обычный адрес и порт.
          </p>
          <button
            type="button"
            className="btn btn-primary w-fit"
            disabled={busy}
            onClick={onEnable}
          >
            {busy ? "Поднимаю туннель…" : "Пустить через релей"}
          </button>
        </div>
      )}

      {enabled && (
        <div className="grid gap-2">
          {address ? (
            <p className="text-sm">
              Адрес для друзей: <code className="font-mono">{address}</code>
            </p>
          ) : (
            <p className="text-sm text-muted">
              {error
                ? `Туннель не поднялся: ${error}`
                : "Поднимаю туннель — панель подключается к релею…"}
            </p>
          )}
          <button
            type="button"
            className="btn btn-ghost w-fit text-sm"
            disabled={busy}
            onClick={onDisable}
          >
            Убрать из релея
          </button>
        </div>
      )}

      {heldBy && !enabled && (
        <p className="text-sm">
          Туннель сейчас занят другим сервером. Он один — за ним один публичный
          порт, и два сервера означали бы игроков одного, приходящих ко второму.
          Включите здесь, и он перейдёт сюда.
        </p>
      )}

      {heldBy && (
        <button
          type="button"
          className="btn btn-ghost mt-2 w-fit text-sm"
          disabled={busy}
          onClick={onEnable}
        >
          Забрать туннель себе
        </button>
      )}
    </section>
  );
}
