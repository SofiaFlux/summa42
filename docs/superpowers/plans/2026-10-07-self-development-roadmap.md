# Summa42: stan projektu i plan dojścia do samodzielnego rozwoju

> Dla wykonawcy: realizuj zadania kolejno według Superpowers `executing-plans`. Przed implementacją porównaj aktualny HEAD z bazą tego raportu. Każdy etap wymaga własnej weryfikacji i review. Dokument jest propozycją architektury i kolejności implementacji; nie oznacza uruchomienia autonomii ani przyznania nowych uprawnień.

**Data:** 7 października 2026. **Baza analizy:** `SofiaFlux/summa42`, `main`, commit `5a633000e849d3f49f5281314a30ebac9e08f73d`.

**Cel:** Summa42 samodzielnie wybiera pracę z autoryzowanego backlogu, przygotowuje zmianę we własnym repozytorium, testuje ją, uzyskuje niezależne review, obsługuje PR/CI/issues, a następnie — w dopuszczonym zakresie — scala zmianę i wdraża nową wersję z możliwością powrotu.

**Architektura:** jeden Maintainer Collective na istniejącym single-Cube kernelu. Deterministyczny driver prowadzi durable WorkflowCase, modele wykonują ograniczone zadania poznawcze, a osobne komponenty obsługują checkout, testy i skutki zewnętrzne. Istniejące Task/Attempt, fencing, evidence, policy, approvals, resources i External Operations pozostają podstawą.

**Stack:** Go 1.27, SQLite, istniejąca OPA, Git, GitHub API, istniejący adapter executora; Linux/OCI tam, gdzie potrzebna jest techniczna izolacja.

**Zakres:** analiza i plan. Nie zmieniono kodu, issues, ustawień GitHuba ani wdrożenia.

## Aktualizacja implementacji — 8 października 2026

PR #19 dostarczył pierwszą podstawę executor eligibility. Otwarty draft PR #20 rozwija nadzorowane przygotowanie źródeł i review planu, lokalną implementację z rzeczywistymi testami, niezależne review kandydata oraz chronioną publikację dokładnego commita jako branch i draft PR. Publikacja wymaga osobnych uprawnień Case, korzysta z dwóch ExternalOperation slots, zachowuje zgody właściciela i nie ponawia niepewnych zapisów. Zobacz [plan publikacji](2026-10-08-github-publication.md).

To nadal komendy nadzorowane, a nie automatycznie złożony worker. Dziś Summa nie uczestniczy w pracy ani publikacji na żywo. Następny etap to niezależny od implementera obserwator wyników CI przypisanych do dokładnego candidate SHA, z trwałym evidence i weryfikacją publikacji. Dopiero później ograniczona naprawa po review/CI, owner-approved merge oraz obsługa stanu issues. PR #20 pozostaje draft; etap publikacji nie przyznaje prawa do merge ani zamykania issues.

Pierwotna analiza poniżej opisuje stan na wskazanej bazie i pozostaje historycznym punktem odniesienia.

## 1. Wniosek

Summa42 ma już znaczną część mechaniki sterowania i trwałości. Najbliższa przeszkoda jest konkretna: przepływ GitHuba dochodzi do planu i propozycji `github.issue.plan.review`, po czym nie ma executora i kontraktu routingu pozwalającego kontynuować. Kolejne brakujące elementy to przygotowanie repozytorium, wykonanie i niezależna weryfikacja zmiany, publikacja PR oraz powrót wyników CI/review do workflow.

Najkrótsza droga to domknięcie jednego cyklu developerskiego. Company OS, A2A, distributed runtime, autonomiczne pozyskiwanie providerów i rozbudowane dreaming nie są warunkami pierwszego samodzielnie przygotowanego PR.

Rekomendowany pierwszy poziom: Summa42 przygotowuje zweryfikowany PR i obsługuje poprawki, a właściciel zatwierdza merge. Docelowy poziom: owner-approved polityka pozwala samodzielnie scalać określone klasy zmian; osobny supervisor wdraża sprawdzony build. Człowiek zajmuje się wyjątkami i kierunkiem, zamiast każdym krokiem pracy.

## 2. Co potwierdziłem

| Obszar | Stan na badanym HEAD | Znaczenie |
|---|---|---|
| Kernel | Task/Attempt, leases/fencing, evidence, acceptance, approvals, operations, resources, run manifests | Podstawa do trwałego procesu już istnieje. |
| Worker | `internal/scheduler/worker.go`, `run-worker` | Potrafi wykonywać zarejestrowane executory. README nadal twierdzi, że worker nie startuje. |
| GitHub intake | `internal/ghissue`, `run-gh-intake` | Czyta otwarte issues, filtruje autorów/assignees/labels, zapisuje rewizje i cases. Klient udostępnia GET, nie writes. |
| Triage | `internal/ghtriage` | Reguły → ograniczona klasyfikacja modelem → deterministyczny disposition. Large/unknown scope prowadzi do needs-human. |
| Review triage | `ghtriage.Reviewer` | Sprawdza strukturę, zgodność stanu i plausibility klasyfikacji. Nie jest review planu ani kodu. |
| Planner | `ghtriage.Planner`, `run-gh-plan` | Zapisuje plan jako evidence i proponuje `github.issue.plan.review`; nie materializuje następnego Tasku. |
| Plan | `ghtriage.PlanInput/PlanOutput` | Snapshot issue + decision, summary i 1–8 kroków. Brak jawnego snapshotu kodu, base SHA, zakresu plików i kontraktu testów. |
| Executor kodujący | `internal/executors/codex.go` | Adapter istnieje. Badana kompozycja `run-worker` rejestruje triage/Copilot/ADO publish; nie podpina Codexa jako implementera issue. |
| Workspace | `Worker.executeAttempt` | Tworzy katalog Attemptu przez `MkdirAll`. Nie wykonuje checkoutu repozytorium. |
| Writes GitHuba | `feedbackgithub` | Istnieje chronione tworzenie sanitized feedback issue. Brak ogólnego cyklu branch/push/PR/review/merge dla samorozwoju. |
| ADO | `adoreview`, `adoeffects` | Są wzorce drivera, publishera i final verifiera. Można wykorzystać semantykę, lecz provider GitHuba wymaga własnej implementacji. |
| CI | Run `36696570907` dla badanego SHA | Zielone test, race, vet, build, bootstrap smoke, SQLite spike oraz OCI acceptance. |
| Repo | Publiczne, tylko branch `main`, protected=true, auto_merge=false | Brak otwartych PR. Szczegóły branch protection niedostępne dla integracji: 403. Lista rulesets jest pusta. |
| Backlog | 12 otwartych issues, wszystkie bez labels i assignees | Backlog zawiera głównie duże kierunki; nie jest jeszcze kolejką małych wykonalnych zadań. |

PR #6 i #7 zostały scalone 22 września. Dawna informacja o niewykonanym CI w ich opisach nie opisuje obecnego HEAD: najnowszy sprawdzony run na main zakończył się sukcesem 30 września.

**Granice dowodu:** przeanalizowałem kod, dokumenty, backlog, PR i wyniki GitHub Actions. Nie uruchamiałem Summa42 na Twoim serwerze, modelu ani rzeczywistym repozytorium roboczym. Zielone CI potwierdza obecny test suite; nie dowodzi działania przyszłego cyklu issue → kod → PR → merge. Nie wykonywałem lokalnie całej macierzy Go.

## 3. Luki, które muszą wejść do planu

### P0 — sterowanie, izolacja i rozliczenie

1. **Routing nie jest twardą kwalifikacją executora.** Worker przekazuje do `ChooseExecutor` wszystkie registry kinds. `TaskClassRouting` działa jako preferencja, a fallback jest alfabetyczny. Dla przyszłego workflow brak właściwego executora musi blokować pracę, a nie uruchamiać inny. Ocena capabilities powinna być per executor i Task.
2. **Capacity zawyża deklarowany poziom enforcement.** `workerCapacity` przypisuje ENFORCED wszystkim registry kinds. Nie dowodzi to technicznego containmentu procesu. Istniejący Codex adapter rozróżnia PARTIAL/ENFORCED; scheduler powinien konsumować rzeczywistą ocenę danego executora i jego capabilities.
3. **Wywołania modelu nie mają jednolitego cyklu.** Planner i reviewer wywołują model bez zwykłego wykonania przez worker. `climodel.Adapter` dziedziczy środowisko procesu i nie ustawia kontrolowanego workspace. Read-only capability w metadanych nie jest dowodem, że uruchomiony CLI nie ma narzędzi, sieci lub ambient credentials.
4. **Usage nie jest pełnym rozliczeniem.** Codex parser zwraca token usage, ale worker zapisuje evidence i CompletionManifest, bez rozliczenia tego Usage. Planner CLI też nie zwraca canonical usage. Workflow RemainingBudget nie należy interpretować jako USD bez udokumentowanego przeliczenia.
5. **Lease i długość pracy.** Domyślny lease to 5 minut, a Worker ogranicza całe wykonanie tym czasem. Implementacja/testy potrzebują świadomego timeoutu albo odnowienia lease z zachowaniem fencing. Samo ustawienie bardzo długiego lease utrudnia wykrycie porzuconej pracy.
6. **Limit workflow.** Intake domyślnie ustawia MaxSteps=3. Pełny cykl review/implementacji/poprawek wymaga większego, nadal ograniczonego budżetu kroków.

Powyższe są stwierdzeniami o badanej ścieżce kodu i ryzykach jej rozszerzenia. Nie twierdzę, że zaobserwowałem naruszenie containmentu albo nieautoryzowany write w działającym środowisku.

### P1 — brakujące wykonanie

- Plan jest oparty na issue, bez jawnie dostarczonego kontekstu repozytorium. To wystarcza do szkicu; nie do autoryzacji implementacji.
- Brakuje review planu, materializacji implementation Task i powiązania patcha z zaakceptowanym planem.
- Brakuje niezależnego uruchomienia testów dla dokładnego candidate SHA i code review tego samego diffu.
- Brakuje protected GitHub writes i odczytu PR/checks/review threads jako nowego wejścia procesu.
- Komendy intake/worker/driver/reviewer/planner są osobnymi trybami; nie ma kompletnego supervised maintainera, który sam prowadzi cały cykl.

### P2 — autonomia po pierwszym PR

- Obecny filtr intake wymaga autora z listy maintainerów. Bot tworzący własne issues potrzebuje jawnego endorsement/provenance; samo dopisanie wszystkich autorów do allowlisty byłoby zbyt szerokie.
- `updated_at` issue jest dziś revision ID. Komentarze i labels utrzymaniowe bota mogą zmieniać tę rewizję. Potrzebny jest content/spec fingerprint odróżniający zmianę wymagania od własnej aktualizacji statusu.
- Brakuje zewnętrznego supervisora nowej wersji Boxa, canary i sprawdzonego rollbacku, również dla zmian schematu SQLite.

## 4. Decyzje architektoniczne

Rozważyłem trzy drogi:

| Droga | Zaleta | Koszt/ograniczenie | Decyzja |
|---|---|---|---|
| Agent CLI steruje wszystkim przez shell i gh | Najszybszy demonstrator | Obchodzi canonical operations, recovery, authority i rozliczenie | Może służyć do ręcznego bootstrapu pod nadzorem; nie jako docelowy maintainer. |
| Maintainer workflow na obecnym kernelu | Wykorzystuje istniejącą trwałość i governance | Trzeba zbudować brakujące adaptery i kontrakty | **Rekomendowane.** |
| Najpierw pełny Company OS/distributed swarm | Duży zakres przyszłych możliwości | Opóźnia pierwszy użyteczny cykl i zwiększa liczbę zależności | Poza krytyczną ścieżką. |

Wybrany podział odpowiedzialności:

- **Driver:** wybiera następny dozwolony krok na podstawie canonical evidence i stanu.
- **Planner/implementer/reviewer:** role poznawcze; początkowo wykonywane sekwencyjnie, z osobnymi kontekstami. Niezależne review nie musi oznaczać innego dostawcy, lecz nie korzysta z niezweryfikowanego zapewnienia autora o poprawności.
- **Workspace i test runner:** deterministyczne przygotowanie repozytorium i sprawdzenie wyniku.
- **GitHub publisher:** przeprowadza dokładnie określone skutki przez External Operations. Model nie dostaje tokenu do push/merge.
- **Supervisor:** poza procesem rozwijanego Boxa; aktywuje wersję i przywraca poprzednią.

Nie budować nowego frameworka agentów. Najpierw użyć jednego już dostępnego executora. Adapter Claude lub kolejny provider jest niezależnym rozszerzeniem, jeśli będzie potrzebny do Twojego sposobu pracy.

## 5. Kontrakt pracy

Każde zadanie implementacyjne wiąże:

`repo + issue + spec_fingerprint + plan_hash + base_sha + allowed_paths + acceptance_commands + budget_envelope + policy_version`.

Artefakt implementacji wiąże:

`task_id + attempt_id + fence + base_sha + candidate_sha + diff_hash + changed_paths`.

Weryfikacja wiąże:

`candidate_sha + test_configuration_hash + command_results + reviewer_verdict + unresolved_findings`.

Zmiana SHA lub wymagań unieważnia odpowiednie zatwierdzenie. Sam wynik modelu CLEAN, stdout „tests pass”, poprawny JSON albo exit code 0 nie wystarczają do Task acceptance.

**Przepływ docelowy:** obserwacja issue → kwalifikacja → checkout i kontekst → plan → review planu → implementacja → deterministyczne testy → niezależne code review → poprawki w limitach → push/PR → odczyt CI/review → merge gate → scalenie → zamknięcie issue → opcjonalne wdrożenie i feedback.

Praca z Gitem oznacza: izolowany checkout na konkretnym SHA, walidację diffu, commity, publikację branch/PR, aktualizację branch na podstawie konkretnego remote SHA, obsługę konfliktów i archiwizację. Żadnego bezwarunkowego force-push, pisania bezpośrednio na main lub automatycznego resetowania cudzej pracy.

## 6. Globalne ograniczenia i progi autonomii

**A0 — bootstrap:** zewnętrzny agent implementuje brakujące części; Summa42 jeszcze nie potrafi wykonać ich samodzielnie.

**A1 — issue → zweryfikowany PR:** samodzielna realizacja zadania i poprawki; człowiek zatwierdza merge. To pierwszy kamień milowy.

**A2 — policy merge:** właściciel uprzednio zatwierdza klasy zmian, ścieżki, koszty i gates. Tylko dopuszczone zmiany mogą być scalane samodzielnie. Branch protection nadal obowiązuje.

**A3 — self-upgrade:** osobny supervisor wdraża zweryfikowane wydanie; health-check/canary oraz rollback. To drugi, osobny problem po source-code self-development.

Na pierwszy pilot proponuję:

- Jedno repo `SofiaFlux/summa42`, jeden aktywny development case; pozostałe mogą czekać w backlogu.
- Pozytywny wybór kolejki przez `summa:ready`, wstrzymanie przez `summa:hold`; labels są projekcją, canonical grant pozostaje w Summa42.
- MaxSteps=24 na development case; najwyżej 2 ponowienia błędu infrastrukturalnego danego kroku i 3 cykle napraw po findings. Wszystko ograniczone wspólnym budżetem.
- Proponowany początkowy limit API: 5 USD na case i 20 USD dziennie. To ustawienia pilota do kalibracji, nie oszacowanie kosztu pracy. Internal monetary ledger w integer micro-USD; nieznane zużycie pozostaje exposure. Przy subskrypcji osobno mierzyć zużycie/quota; nie udawać kosztu 0.
- Egzekwować limit czasu i kosztu przed kolejnym wywołaniem. Jeśli provider nie umożliwia twardego capu, jawnie deklarować enforceability i rezerwować konserwatywną ekspozycję.
- Początkowo ścieżki owner keys, policy/authority, approvers, spending configuration, deployment credentials i branch protection wymagają właściciela. Zmiany wykonania GitHub Actions również do osobnego review przed A2.
- Agent może zgłosić potrzebę szerszego scope; nie może przyznać go sam sobie. Brak postępu, nieznany outcome i niespełnione gates mają widoczny powód oraz termin następnego działania.

## 7. Plan wykonania — niezależnie odbierane etapy

Każdy pakiet realizować: test odtwarzający wymaganie → potwierdzenie FAIL → minimalna implementacja → PASS → review → commit. Nazwy nowych plików i komponentów poniżej są propozycją do implementacji, nie opisem istniejącego kodu. Etapy wymagają szczegółowego speca ograniczonego do ich zakresu, bez ponownego projektowania całego kernela.

### Etap 0: aktualny obraz systemu i kolejka pilota

**Pliki:** `README.md`, nowy `docs/maintainer-runbook.md`, `cmd/summa42-box/main.go` tylko w zakresie dokumentowania istniejących trybów.

- [ ] Uzupełnić README o istniejące komendy worker/intake/triage/review/plan oraz dokładny punkt zakończenia obecnego przepływu.
- [ ] Zestawić konfigurację modelu, repo, grantów, resource envelope i wymagane uprawnienia bez ujawniania credentials.
- [ ] Wybrać 3–5 małych zadań pilota: dokumentacja obecnych komend, mały bug z reprodukcją i testem, drobna poprawa CLI. Duże issues rozbić na child issues o jednym rezultacie i jawnych zależnościach.
- [ ] W runbooku podać start/stop/status, backup i odczyt powodu zablokowania case. Sprawdzić GitHub App oraz faktyczne required checks/reviews w ustawieniach administracyjnych, do których ta integracja nie ma dostępu.

**Odbiór:** drugi operator potrafi wskazać, jakie komponenty rzeczywiście uruchamia dana komenda i czego jeszcze nie robi. Kolejka zawiera mierzalne, małe zadania.

### Etap 1: twardy kontrakt executora i rozliczanie

**Zmienić:** `internal/scheduler/worker.go`, `service.go`, `routing.go`, `internal/executors/executor.go`, `internal/resources/service.go`, `cmd/summa42-box/main.go`.

**Dodać:** `internal/scheduler/eligibility.go`, `internal/executors/accounting.go` oraz odpowiadające testy.

- [ ] Zdefiniować descriptor executora: obsługiwane TaskClass, semantic capabilities, faktyczny enforcement i koszt/usage contract. Eligibility wyliczać dla konkretnego Tasku przed preferencją i Lease.
- [ ] Brak pasującego executora ma zwracać unschedulable z powodem. Nie wybierać alfabetycznego fallbacku dla klasy wymagającej jawnego kontraktu.
- [ ] Capacity pochodzi z assessmentu konkretnego executora; Partially enforced proces nie staje się ENFORCED przez samo wpisanie do registry.
- [ ] Wszystkie produkcyjne wywołania triage/planning/review podpiąć pod governed wykonanie: resource reservation, Attempt provenance, timeout, cancellation, usage settlement. Komendy manualne mogą pozostać narzędziami diagnostycznymi.
- [ ] Zapewnić kontrolowane środowisko/CWD/tool access dla model adaptera i brak Owner key/GitHub write token w procesie poznawczym.
- [ ] Wprowadzić odnowienie lease lub deterministyczny podział długiej pracy; cancellation obejmuje cały process tree. Stary fence nie może opublikować wyniku.

**Testy:** `TestMissingExecutorBlocksClass`, `TestPartialExecutorCannotSatisfyEnforcedTask`, `TestUsageSettledOnceAfterRestart`, `TestUnknownUsageRetainsExposure`, `TestExpiredFenceCannotPublish`, `TestModelCannotReadOwnerMaterial`.

**Weryfikacja:** `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler ./internal/executors ./internal/resources ./cmd/summa42-box -count=1 -timeout 5m`; wynik PASS. Dodatkowo acceptance containmentu w docelowym środowisku, bo same mocks nie dowodzą izolacji.

**Odbiór:** zadanie nigdy nie trafia do niewłaściwego executora, a model calls nie obchodzą budżetu i trwałości.

### Etap 2: repo snapshot, plan review i implementation handoff

**Zmienić:** `internal/ghtriage/plan.go`, `planner.go`, `planning.go`, `internal/workflowcase/materialize.go`.

**Dodać:** `internal/workspace/git.go`, `manifest.go`, `internal/maintainer/plan_review.go`, `contracts.go` i testy.

- [ ] Workspace przygotowuje izolowany checkout na base SHA; źródłowa kopia repo nie jest współdzielonym katalogiem zapisu między Attempts. Repo URL pochodzi z zatwierdzonej konfiguracji.
- [ ] Dostarczyć plannerowi jawny context manifest: HEAD, AGENTS, relewantne pliki i testy, dokumenty wymagań/dependencies oraz limit rozmiaru. Wersjonować i hashować ten input.
- [ ] Dodać plan schema v2 z base SHA, scope/allowed paths, acceptance commands i krokami. Zachować możliwość czytania starych evidence v1; nie modyfikować ich.
- [ ] Zaimplementować `github.issue.plan.review`: niezależny review modelu plus deterministyczne sprawdzenie scope/grant/budget/komend; wyniki ACCEPT, REVISE, BLOCK z evidence.
- [ ] ACCEPT materializuje `github.issue.implement` przez istniejący WorkflowCase contract, tylko dla aktualnej rewizji i właściwego plan_hash. Review planu nie jest nowym źródłem uprawnień.
- [ ] Jawnie podnieść limity development recipe; nie zmieniać globalnie limitów innych recipes.

**Testy:** `TestCheckoutPinnedToBaseSHA`, `TestPlanReviewRejectsScopeExpansion`, `TestPlanV1RemainsReadable`, `TestNewIssueSpecInvalidatesHandoff`, `TestMissingPlanReviewerDoesNotMaterializeImplementation`.

**Weryfikacja:** testy `./internal/workspace ./internal/ghtriage/... ./internal/maintainer ./internal/workflowcase`, count=1, timeout 5m.

**Odbiór:** zaakceptowany, code-grounded plan tworzy dokładnie jedno implementation work; powtórny tick/restart nie tworzy duplikatu.

### Etap 3: implementacja, niezależne testy i code review

**Zmienić:** `internal/executors/codex.go`, `internal/scheduler/worker.go` tylko przez nowe jawne kontrakty.

**Dodać:** `internal/maintainer/implement.go`, `verify.go`, `code_review.go`, `repair.go`, `internal/workspace/diff.go`, `tests/acceptance/maintainer_development_test.go`.

- [ ] Podpiąć implementera do task registry/routing. Objective zawiera zaakceptowany plan, a Payload/context przekazuje hash-bound snapshot i kryteria; nie polegać na samym ogólnym codexPrompt.
- [ ] Agent zmienia pliki lokalnie bez zewnętrznych write credentials. Zaufany wrapper zbiera i waliduje diff: allowed paths, symlinks, submodules, rozmiar, secrets oraz zgodność base SHA. Tworzy immutable candidate artifact/commit.
- [ ] Weryfikator przygotowuje świeży checkout candidate SHA i sam uruchamia zatwierdzone testy. Repozytorium z agentowym kodem testowym jest wykonywalnym wejściem: runner nie ma Owner keys, GitHub write token ani innych ambient credentials.
- [ ] Review code czyta exact diff/base/candidate oraz wyniki testów. Findings muszą wskazywać plik, powód i evidence; UNCERTAIN blokuje acceptance.
- [ ] Repair tworzy nowy Attempt na nowym kandydacie; test/review evidence starego SHA nie jest przenoszone jako zatwierdzenie nowego. Po 3 cyklach albo braku postępu praca blokuje się z diagnozą.

**Testy:** `TestOutOfScopePatchRejected`, `TestAgentTestsPassClaimIgnored`, `TestReviewBoundToCandidateSHA`, `TestRepairCannotReuseOldAcceptance`, `TestNoProgressStopsWithinBudget`, `TestTestRunnerHasNoPublisherCredentials`.

**Weryfikacja:** package tests maintainer/workspace/executors oraz acceptance. Dla zmian Go pełne `go test`, `go test -race`, `go vet`, `go build`; dla zmian kernela również istniejące persistence/OCI gates.

**Odbiór:** realny model poprawia mały reprodukowalny bug, niezależny runner potwierdza regresję i suite, reviewer ocenia ten sam candidate SHA. Na tym etapie istnieje zweryfikowana lokalna zmiana.

### Etap 4: protected GitHub publisher i milestone A1

**Dodać:** `internal/githubeffects/client.go`, `provider.go`, `intents.go`, `reconcile.go`, `internal/maintainer/publish.go`, `pr_observer.go` i testy.

**Wykorzystać:** `internal/operations.Provider`, stable effect slots, reservations i LookupOutcome; wzorce `feedbackgithub` i `adoeffects`.

- [ ] Udostępnić typed effects: utworzenie branch/ref, publikacja wskazanego candidate commit, utworzenie/aktualizacja PR, komentarz statusowy, labels i zamknięcie issue. Każdy effect ma osobny capability i canonical intent.
- [ ] Publisher sprawdza repo, prefix branch, base/candidate SHA i dowody acceptance; token pozostaje w zaufanym komponencie. Ewentualny Git transport jest kontrolowany przez publishera, nie shell modelu.
- [ ] Ref update wymaga expected remote SHA. Obiekt/blob publication można deduplikować po content hash; widoczny effect branch/PR wymaga odczytu i reconciliation po niepewnym wyniku.
- [ ] PR zawiera issue linkage, opis zachowania, candidate SHA, test/review evidence i ograniczenia. Odczytywać checks, reviews, threads, branch state oraz merge status.
- [ ] Findings/failed CI wracają do ograniczonego repair workflow. Błąd infrastruktury CI rozróżniać od regresji kodu; nie naprawiać kodu na podstawie samego timeoutu runnera.
- [ ] Własne status comments/labels nie powodują bez końca nowego triage/planning. Spec fingerprint uwzględnia zmianę wymagań i autoryzowane decyzje, nie każde dotknięcie updated_at.

**Testy:** `TestPRCreateTimeoutReconcilesWithoutDuplicate`, `TestPushRequiresExpectedRemoteSHA`, `TestCommentUpdateDoesNotRestartImplementation`, `TestChangedRequirementInvalidatesCandidate`, `TestFailedCIProducesBoundedRepair`, `TestIssueNotClosedBeforeVerifiedDelivery`.

**Odbiór A1:** od jednego owner-endorsed issue do zweryfikowanego PR z CI i obsługą co najmniej jednego cyklu findings → repair. Restart pomiędzy push a zapisem wyniku nie tworzy drugiego PR ani drugiego skutku.

### Etap 5: jeden supervised maintainer i backlog management

**Dodać:** `internal/maintainer/driver.go`, `recipe.go`, `intake.go`, `status.go`, osobny `cmd/summa42-box/maintainer_cmd.go`, `docs/maintainer-runbook.md`.

**Zmienić:** `internal/ghissue` i istniejące control/status tylko w potrzebnym zakresie; trzymać nowy CLI poza dalszym rozrostem `main.go`.

- [ ] `run-maintainer` prowadzi wszystkie etapy przez canonical transitions. Każdy tick ma trwały powód następnego działania; scheduler nie zależy od ręcznego odpalania 5 komend.
- [ ] Zapewnić jednego koordynatora state transitions/publishing dla danego Collective. Executor może działać osobno; nie zakładać, że dowolne procesy otwierające tę samą SQLite automatycznie zapewniają single-writer orchestration.
- [ ] Heartbeat/status pokazuje active case, step, candidate SHA, budżet, exposure, blocked reason i następny retry. Pause wstrzymuje nowe work; stop anuluje procesy i odbiera zdolność dalszego dispatch.
- [ ] Kwalifikacja backlogu: ready + endorsement + dependencies + dopuszczony scope; rekomendowane priorytety: reprodukowalne bugi, brakujące tests/gates, małe funkcje, refactor powiązany z zadaniem.
- [ ] Summa może rozbić duże issue, wykryć duplicate i utworzyć propozycję child issue. Auto-created work staje się wykonywalne tylko według zatwierdzonego generatora/polityki, z provenance i w granicach allowlisty. Autor-bot sam w sobie nie nadaje uprawnień.
- [ ] Ograniczyć polling/backoff, obsłużyć GitHub rate limit, PR zamknięty ręcznie, issue closed/reopened, konflikt base i równoległą zmianę człowieka. Labels są projekcją, nie źródłem prawdy.

**Testy:** restart przy każdym przejściu; `TestTwoDriversDoNotPublishTwice`, `TestUnendorsedBotIssueNotExecuted`, `TestHumanEditSupersedesPlan`, `TestClosedIssueStopsPendingWork`, `TestRateLimitBackoff`, `TestPausePreventsNewDispatch`.

**Odbiór:** 24–48 godzin pilota bez ręcznego przepychania etapów, z jawnym raportem wszystkich interwencji. Następnie 7 dni dogfoodingu dla kalibracji kosztu i blokad.

### Etap 6: policy merge, milestone A2

**Dodać:** `internal/maintainer/merge_gate.go`, typed merge effect w `githubeffects`, osobny owner-approved policy profile i testy.

- [ ] Wykonać 10 małych zadań pilota A1; żadne nie może zostać zaakceptowane z pominięciem wymaganych gates. Rejestrować interwencje, koszty i reopen/regressions.
- [ ] Gate wymaga aktualnego candidate SHA, zielonych required checks, review tego SHA, rozwiązanych blokujących findings, aktualnego planu/polityki, dozwolonych ścieżek i braku unknown external effects.
- [ ] Merge wykonuje GitHub provider z warunkiem expected head SHA. Jeśli main przesunął się i wpływa to na wynik, aktualizacja branch generuje nowy candidate oraz ponowną weryfikację; nie odziedzicza starego CLEAN.
- [ ] Potwierdzić merge commit i relację do planu/candidate. Issue zamykać po potwierdzonej dostawie zgodnie z jego acceptance, nie po samym wysłaniu requestu merge.
- [ ] Uwzględnić faktyczne branch protection. Jeśli wymagany approval musi pochodzić od innego konta/principal niż autor PR, użyć zatwierdzonego review identity albo pozostać przy A1; nie osłabiać ochrony dla obejścia limitu.

**Testy:** `TestNewHeadInvalidatesMergeApproval`, `TestPendingOrMissingRequiredCheckBlocksMerge`, `TestUnresolvedReviewBlocksMerge`, `TestMergeTimeoutReconcilesCommit`, `TestProtectedPathNeedsOwner`.

**Odbiór A2:** trzy małe, dozwolone zmiany przechodzą pełny cykl bez interwencji w rutynowych krokach; zmiana chroniona poprawnie czeka na właściciela.

### Etap 7: self-upgrade, milestone A3

**Dodać:** osobny `cmd/summa42-supervisor`, `internal/upgrade/manifest.go`, `health.go`, `rollback.go`, `docs/upgrade-runbook.md`.

- [ ] Build/release z dokładnego merged SHA ma hash oraz deklarowany zakres zgodności DB/config. Running Box nie nadpisuje własnego binarium podczas pracy.
- [ ] Supervisor zatrzymuje nowe leases, kończy albo anuluje aktywne Attempts i uzgadnia unknown effects przed zmianą wersji; stan work pozostaje trwały.
- [ ] Backup SQLite wykonać przez wspierany snapshot/backup przy prawidłowej obsłudze WAL. Test upgrade i restore prowadzić na kopii z aktywnymi tasks i operations.
- [ ] Canary sprawdza startup, policy provenance, odczyt danych, scheduling i prosty synthetic case. Nie tylko odpowiedź status endpoint.
- [ ] Przy błędzie uruchomić poprzedni build wyłącznie na zgodnej bazie. Nie odtwarzać starej DB po zewnętrznych skutkach nowej wersji bez reconciliation: mogłoby to odtworzyć już wykonane operacje.
- [ ] Dla breaking migrations wymagać owner-reviewed migration/rollback planu. Preferować kompatybilne expand-contract migrations, aby binary rollback nie wymagał cofania historii operations.

**Testy:** `TestBadBuildRollsBackBeforeEffects`, `TestRollbackDoesNotReplayPublishedPR`, `TestMigrationIncompatibilityBlocksUpgrade`, `TestUpgradePreservesPendingCases`, `TestSupervisorSurvivesBoxFailure`.

**Odbiór A3:** Summa wdraża własną dozwoloną zmianę, wykonuje następne zadanie na nowej wersji i przechodzi kontrolowany test awarii/rollbacku bez utraty canonical state i bez duplikowania skutków.

## 8. Powiązanie z obecnymi issues

| Issue | Decyzja dla samorozwoju |
|---|---|
| #1 field dogfooding | Duża część kodu jest już na main. Zweryfikować acceptance criteria i live smoke przed aktualizacją statusu; wykorzystać obserwacje do backlogu. Nie zakładać automatycznie pełnego zamknięcia. |
| #14 Summand vocabulary | Kandydat na mały docs pilot po ścisłym ograniczeniu scope. Sam rename nie jest dowodem zdolności do implementacji buga. |
| #4 replay/regression | Wyciągnąć minimalny wycinek: odtwarzanie i failure/restart harness jednego maintainer workflow. Pełny replay system może poczekać. |
| #5 rogue containment | Potrzebne minimum pause/stop/revoke/credential separation w pilocie; pełna warstwa rogue detection później. |
| #15 learning/dreaming | Zacząć od wynik → evidence → propozycja issue. Automatyczne zmiany procesu/polityki wymagają osobnych gates; pełne dreaming po działającym cyklu. |
| #12 intent compiler | Tymczasowo structured issue contract i prosta dekompozycja. Nie budować pełnego compiler przed pierwszym PR. |
| #2 capability discovery | Nie na krytycznej ścieżce. Wybrać istniejącego executora; acquisition nowych narzędzi nie jest warunkiem działania. |
| #3 OSS/control plane | Zachować architekturę i oceniać konkretne zależności według potrzeby; nie robić rewrite w ramach bootstrapu. |
| #11 DTU | Deterministyczne fake providers i failure injection wystarczą na pierwszy cykl. |
| #16 decision engine | Obecne reguły + model interfaces wystarczą; future engine może zastąpić konkretną rolę. |
| #17 A2A, #18 Company OS | Poza etapami 0–7. Maintainer Collective może działać bez profilu Company OS. |

Proponowany nowy epic: **Maintainer Collective — bounded issue-to-release self-development**. Child work odpowiada etapom 1–7, z zależnościami; do gotowej kolejki wpuszczać dopiero małe zadania mające działające prerequisites. Nie tworzyłem tych issues w tej analizie.

## 9. Kryteria sukcesu i raport operacyjny

Minimum raportu po każdym case: issue/plan/candidate/PR/merge SHA, test i review evidence, koszt zmierzony i unknown exposure, retries, czas oczekiwania oraz liczba i powód interwencji człowieka.

Mierzyć:

- odsetek autoryzowanych małych issues zakończonych zweryfikowanym PR;
- odsetek zmian scalonych i niewymagających reopen/regression;
- interwencje właściciela na zadanie, rozdzielone na decyzje produktowe i techniczne przepychanie procesu;
- koszt na zaakceptowaną zmianę, również wszystkich nieudanych Attempts;
- duplicate external effects — docelowo 0;
- blocked cases bez powodu lub następnego działania — docelowo 0;
- liczbę bounded repairs i powtarzanych findings.

Deklaracja „Summa42 rozwija Summa42” jest uzasadniona na A1 dla tworzenia zmian/PR. Deklaracja „sama utrzymuje repo i własne wydania” wymaga A2 oraz A3. Wizja około godziny obsługi miesięcznie potrzebuje dłuższego okresu stabilnej eksploatacji; 7-dniowy pilot nie jest takim dowodem.

## 10. Główne miejsca do review

1. Stare SHA/revision/fence próbujące zatwierdzić lub opublikować nowszy rezultat — etap 1/2/3/6.
2. Timeout po wykonanym skutku zewnętrznym — etap 4/6/7: odczyt i reconciliation zamiast blind retry.
3. Model/test runner dziedziczący authority lub credentials — etap 1/3.
4. Własne komentarze, labels i bot-generated issues powodujące pętle lub niezamierzone autoryzacje — etap 4/5.
5. Upgrade/rollback DB odtwarzający wykonane wcześniej efekty — etap 7.

Nie poszerzać coverage „na wszelki wypadek”: każdy test powinien odtwarzać konkretny warunek kontraktu lub ryzyko wymienione powyżej.

## 11. Źródła i odtwarzalność analizy

Wszystkie ścieżki źródłowe z sekcji 2–3 czytano na bazowym SHA. Repozytorium: https://github.com/SofiaFlux/summa42/tree/5a633000e849d3f49f5281314a30ebac9e08f73d

- CI i kroki obu jobs: https://github.com/SofiaFlux/summa42/actions/runs/36696570907
- Planner i istniejąca decyzja o zatrzymaniu handoff: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/docs/superpowers/plans/2026-09-29-github-issue-planner.md
- Planner contract: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghtriage/plan.go
- Worker/workspace: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/scheduler/worker.go
- Startup/routing/capacity: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/cmd/summa42-box/main.go
- Model adapter: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghtriage/climodel/adapter.go
- GitHub read-only intake: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghissue/client.go
- External-operation contract: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/operations/provider.go
- Backlog: https://github.com/SofiaFlux/summa42/issues

**Następne konkretne zadanie:** etap 1 — hard executor eligibility, rzeczywista capacity i accounting — równolegle z dokumentacyjnym uporządkowaniem etapu 0 tylko jeśli wykonawcy mają niezależny zakres plików. Następnie etap 2 domyka obecny `github.issue.plan.review`. Pierwsze uruchomienie zewnętrznych GitHub writes następuje dopiero po odbiorze tych kontraktów i lokalnego cyklu z etapu 3.
