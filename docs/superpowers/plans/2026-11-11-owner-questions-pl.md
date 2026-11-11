# Pytania do Ownera do planu rozwoju z 11 listopada 2026

Dotyczy: [plan rozwoju](2026-11-11-development-plan.md).

Stan na 11 listopada 2026: Owner odpowiedział na pytania kluczowe w rozmowie.
Każde pytanie ma poniżej **Decyzję** (odpowiedź Ownera) albo **Przyjęto
domyślnie** (nie pytano osobno; obowiązuje, dopóki Owner nie zmieni). Zmiana
decyzji: popraw wpis i zrób commit.

## A. Produkt i firma

**Q1.** Co znaczy DaaS w produkcie?
Decyzja: Data as a Service.

**Q2.** W jakiej formie klient dostaje produkt?
Decyzja: API oraz portal (dashboard, aplikacja web).

**Q3.** Co obiecujesz klientom (świeżość, kompletność, dostępność)?
Decyzja: na razie nic; nie ma formalnego SLA, ale w umowach będzie. Najpierw
mierzymy, progi ustawia się później.

**Q4.** Gdzie produkt działa?
Decyzja: Azure.

**Q5.** Jak wygląda wdrożenie produktu?
Decyzja: Azure Pipelines (ADO).

**Q6.** Gdzie jest kod produktu?
Decyzja: GitHub i Azure DevOps, wiele repozytoriów.

**Q7.** Czy poza Ownerem ktoś ma dostawać zadania lub decyzje od systemu?
Decyzja: nie, tylko Owner.

**Q8.** Pierwsze realne zobowiązanie cykliczne?
Decyzja: comiesięczna aktualizacja zależności (najpierw `summa42`, potem
repozytoria produktu).

**Q9.** Którędy przychodzą zgłoszenia klientów?
Decyzja: e-mail oraz formularz kontaktu w portalu, który zapisuje zgłoszenie w
bazie produktu. W przyszłości endpoint w API do zgłaszania zapotrzebowania (na
przykład na dane firmy X); pójdzie przez ten sam system zgłoszeń (portal, API,
baza).

**Q10.** Czym fakturujesz?
Decyzja: inFakt, zautomatyzowane i połączone z produktem, na razie sandbox.
Jest abstrakcja dostawcy, więc da się przepiąć na innego, o ile ma możliwości
integracji. Summa42 obserwuje, nie wystawia.

**Q11.** Czy system może sam pisać do klientów albo wystawiać dokumenty
finansowe?
Decyzja: na początku nie. Faktury na pewno będą zautomatyzowane, komunikacja w
przyszłości być może też; każde wymaga osobnej, podpisanej delegacji.

**Q12.** Czy prywatne dane i kod mogą trafiać do modeli w chmurze?
Decyzja (zmieniona 11 listopada 2026 przy specyfikacji piaskownicy): trzy
klasy danych. Kod publiczny: dowolny model. Kod prywatny (repozytoria
produktu): może iść do modeli z subskrypcji, także w piaskownicy agentowej.
Dane klientów (zgłoszenia, rekordy, wszystko z bazy produktu): tylko modele z
zerową retencją (ZDR) albo lokalne i nigdy w piaskownicy.

## B. Projekty

**Q13.** Jakie Projekty na początku?
Decyzja: `summa42` oraz jeden projekt `daas` obejmujący wiele repozytoriów.

**Q14.** Drugie źródło pracy?
Decyzja: work itemy Azure DevOps. Do planu doszedł kamień milowy M2A (pełna
ścieżka work item, implementacja, draft PR w ADO), po M2 i przed M5. Lokalna
skrzynka plików zostaje jako mała, opcjonalna karta M2-T7.

**Q15.** Czy Projekty mogą dzielić budżet?
Przyjęto domyślnie: nie; każdy ma własną kopertę pod kopertą Kolektywu.

**Q16.** Czy Projekt ma własnych maintainerów innych niż Owner?
Przyjęto domyślnie: nie (zgodne z Q7).

**Q17.** Kiedy praca usługowa dla klientów?
Przyjęto domyślnie: poza tym planem.

**Q18.** Czy wszystko działa na jednym hoście?
Przyjęto domyślnie: tak, jeden host, jedna baza SQLite.

**Q19.** Czy nazwa „Project” jest w porządku?
Przyjęto domyślnie: tak.

## C. Budżet i modele

**Q20.** Limity pieniężne na start?
Decyzja: 5 USD na case i 20 USD dziennie na cały Kolektyw, do kalibracji po
pilotażu.

**Q21.** Jakie subskrypcje i dostępy do modeli są dostępne?
Decyzja: Gemini przez Antigravity (`agy`), Claude, Copilot, Command Code z
subskrypcją Go oraz opencode na darmowych limitach i darmowych modelach;
lokalnie Ollama. Codex nie jest na liście. Nic nie jest dziś płacone za tokeny
przez API.

**Q22.** Czy wolno używać płatnych modeli przez API?
Decyzja: tak, w ramach limitów z Q20.

**Q23.** Jak uruchamiany jest wykonawca i kto recenzuje?
Decyzja: na początek Gemini Flash przez `agy`; inne modele przez Command Code
albo opencode. W przyszłości także dostawcy API (Microsoft Foundry, Vertex,
Bedrock, OpenAI API, Anthropic API) i kolejne harnessy (na przykład Pi); stąd
karta M3-T6. Recenzje wszystkich PR: Claude.

## D. Ciągłość

**Q24.** Kanał powiadomień?
Decyzja: webhook Discorda, za interfejsem nadawcy.

**Q25.** Horyzont nieobecności Ownera?
Decyzja: 7 dni.

**Q26.** Ochrona klucza Ownera?
Decyzja: teraz klucz zaszyfrowany hasłem, odszyfrowywany tylko w CLI na czas
jednej podpisanej akcji; z warstwą abstrakcji (interfejs podpisującego), żeby
później łatwo dodać klucz sprzętowy, SSO i inne formy.

**Q27.** Kopie zapasowe?
Decyzja: codziennie, lokalnie oraz do Azure Blob.

**Q28.** Host docelowy?
Decyzja: maszyna wirtualna z Linuxem i systemd. Ma być uniwersalnie: dziś
głównie Azure, być może darmowa maszyna Oracle; nic zależnego od jednej chmury.

**Q29.** Ile uwagi miesięcznie?
Przyjęto domyślnie: cel około godziny miesięcznie; stan obecny zmierzy pilotaż
(M1-T5).

## E. Automatyzacja

**Q30.** Kiedy system proponuje stałą delegację?
Decyzja: po 5 zatwierdzeniach tej samej klasy w 30 dni, bez odrzuceń.

**Q31.** Ile zgodnych przypadków w trybie cienia?
Decyzja: 5 kolejnych; jedna niezgodność zeruje licznik.

**Q32.** Co system przejmuje sam na start (pierwszy mandat)?
Decyzja: tylko ryzyko LOW, odwracalne, bez skutku na zewnątrz. Wszystko inne
jest propozycją.
Doprecyzowanie po recenzji planu (potwierdzone przez Ownera 11 listopada
2026: rutynę system przyjmuje sam, decyzje po jednym podpisie Ownera): Wizja
(art. 7 i 27) mówi, że dowód daje tylko propozycję, a kopertę uprawnień
powiększa wyłącznie Owner. Dlatego w ramach mandatu system sam przyjmuje tylko
harmonogramy i szablony zadań dla pracy, która już dziś nie wymaga
zatwierdzenia. Każda stała delegacja (przejęcie decyzji, którą dotąd
zatwierdzał Owner), zmiana manifestu i zasilenie budżetu to zawsze propozycja
wymagająca jednego podpisu Ownera. Szerszy wariant wymagałby zmiany Wizji przez
Ownera.

**Q33.** Czego nie wolno delegować nigdy?
Przyjęto domyślnie: polityki, kluczy, listy zatwierdzających, budżetów, plików
workflow CI i definicji pipeline'ów w każdym repozytorium oraz samego
mechanizmu delegacji i mandatu. Komunikacja z klientami
i dokumenty finansowe nie są na tej liście (Q11): są poza mandatem na start, a
Owner może je później delegować osobnym podpisem.

**Q34.** Czy model może podpowiadać kandydatów do automatyzacji?
Przyjęto domyślnie: dopiero po wersji deterministycznej (M6-T6), jako kandydat
przechodzący tę samą ścieżkę.

**Q35.** Jak często podsumowanie?
Przyjęto domyślnie: raz w tygodniu.

## F. Repozytorium i proces

**Q36.** Kto pisze specyfikacje kart `DESIGN`?
Decyzja: Claude; Owner zatwierdza. Gemini Flash wykonuje karty `BUILD`.

**Q37.** Czy do A2 każdy merge robi Owner?
Przyjęto domyślnie: tak.

**Q38.** Co z PR #176 (Dapr i AgentLedger)?
Decyzja: zamknąć bez merge; gałąź zostaje.

**Q39.** Co z epikami „Post-MVC”?
Przyjęto domyślnie: zostają otwarte; sekcja 9 planu mówi, które karty spełniają
ich kryteria zamknięcia.

**Q40.** Kolejność: Projekty przed ciągłością?
Decyzja: tak.

**Q41.** Gdzie żyje konfiguracja firmy (manifesty, zobowiązania, mandaty)?
Przyjęto domyślnie: poza publicznym repozytorium, w `SUMMA42_HOME`; w
repozytorium tylko przykłady.

**Q42.** Język dokumentów w repozytorium?
Przyjęto domyślnie: angielski; ten plik jest po polsku, bo jest dla Ownera.

## G. Issues i dyskusje

**Q43.** Czy przyjęta automatyzacja z M6 jest pierwszą wersją Constructora
(dyskusja #136)?
Decyzja: tak, jako minimalny Constructor v1. Specyfikacja M6-T1 odpowiada na
dziewięć pytań z dyskusji.

**Q44.** Kto i kiedy zakłada issue dla kart?
Decyzja: issue dla kart M0 założone 11 listopada 2026. Kolejne partiami per
kamień milowy, gdy zależności karty są zmergowane; `summa-auto` tylko dla kart
w trybie `pipeline`.

**Q45.** Czy nierozstrzygnięte pytania projektowe zaczynają jako dyskusja?
Przyjęto domyślnie: tak.

**Q46.** Czy epiki zamyka wyłącznie Owner, po spełnieniu kryterium?
Przyjęto domyślnie: tak.

**Q47.** Czy dyskusje #137 i #138 zostają otwarte jako kierunek?
Przyjęto domyślnie: tak; M3-T5 realizuje jednohostową wersję #137.

## H. Cel założycielski (dopisany przez Ownera 11 listopada 2026)

Jednym z celów przy tworzeniu założeń było zapobieganie temu, o czym ostatnio
głośno: agentom, które się „wyrywają” i sieją spustoszenie, oraz agentom, które
komunikują się ze sobą w pokrętny sposób poza kontrolą.

Plan zapisuje to jako sekcję 2.5 i regułę R12. Zasada: wynik agenta jest tylko
propozycją albo dowodem przekazanym Boxowi; agenci nie rozmawiają ze sobą;
najpierw izolacja, dopiero potem wykrywanie. Luki znalezione przy tej okazji i
ich karty: producent i recenzent działają dziś jako ten sam użytkownik systemu
(M9-T6), procesy modeli mają nieograniczoną sieć (M9-T7), nic nie sprawdza
instrukcji i ukrytych znaków przemycanych w wyniku producenta do recenzenta
(M9-T8). M9-T6 i M9-T8 są warunkiem wejścia w stałe delegacje (M5).

Drugi cel, równoważący pierwszy: zabezpieczenia nie mogą zmniejszać możliwości
agentów. Skoro współczesny model w swoim harnessie potrafi z jednego promptu
przez kilka godzin zbudować całą aplikację, Summand ma umieć to samo. Plan
zapisuje to jako sekcję 2.6 („ograniczaj promień rażenia, nie agenta”) i
kamień milowy M1A: pełne narzędzia i godziny pracy wewnątrz piaskownicy,
kontrola na jej krawędzi, pomiar parytetu z gołym harnessem (M1A-T6).

## I. Rozszerzenia wybrane przez Ownera 11 listopada 2026

Decyzja: do planu dochodzą: pamięć projektu (M2-T8), praca niekodowa (M2-T9),
bramka nowych zależności (M1A-T7), routing modeli według klasy zadania
(M3-T7), ćwiczenia awaryjne (M3-T8), decyzje z telefonu (M4-T5) i zlecanie
pracy jednym zdaniem (M4-T6). Panel webowy nie wchodzi do planu.

## J. Decyzje przy otwarciu PR #184

Decyzja: tabela luk w izolacji (sekcja 2.5 planu) zostaje w publicznym
repozytorium; wykonawca kart musi ją znać.

## K. Decyzje do specyfikacji M9-T6 i M1A-T1 (11 listopada 2026)

Decyzje: token dostawcy w kontenerze dozwolony, z osobnym loginem tylko do
piaskownicy, dla `summa42` i dla kodu `daas`; kod pisze Gemini, recenzuje
Claude albo Command Code zależnie od złożoności zadania; użytkownik recenzenta
to `summa42-review`, a zadania sprzed zmiany wytwarza się ponownie; brak
`hidepid` na hoście to ostrzeżenie, nie odmowa; kontenery w trybie rootless;
limit jednego przebiegu agenta to 8 godzin; obraz piaskownicy buduje Owner z
Dockerfile w repozytorium.
