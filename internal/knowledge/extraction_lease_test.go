package knowledge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// extraction_lease_test.go 守作业的**归属权**（010 T016）。
//
// ⭐ 这一段防的事故是整个 Phase 3 里最贵的一种：租约过期被别的 worker 抢走，
// 而旧 worker 不知道，继续把同一批 chunk 送进模型。钱花两遍，账只记一遍，
// 而且**两个 worker 都工作正常**——没有报错、没有异常日志，只有账单不对。
//
// epoch 是唯一能区分"我还是不是持有者"的东西。⚠️ 它**只约束数据发布**：
// 旧 worker 那次外部调用该花的钱已经花了，attempt 照记。把 epoch 当成
// "所以那次调用不算数"就是在系统性低估成本。

func claimTestJob(t *testing.T, repo *Repository, docID, jobID string) RelationExtractionJob {
	t.Helper()
	seedNarrativeDocument(t, repo, docID, 2)
	job, err := repo.initializeExtractionJob(t.Context(), newExtractionJobSpec(jobID, docID, 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// TestClaimIncrementsEpochAndTakesLease——抢占成功就 +1，并写下租约。
func TestClaimIncrementsEpochAndTakesLease(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-claim", "job-claim")

	e1, ok, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil || !ok {
		t.Fatalf("首次抢占失败：ok=%v err=%v", ok, err)
	}
	if e1 != 1 {
		t.Errorf("epoch = %d, want 1", e1)
	}
	// 租约还在，第二个 worker 抢不到。
	if _, ok, err := repo.claimExtractionJob(ctx, job.ID, time.Minute); err != nil || ok {
		t.Errorf("租约有效期内被第二个 worker 抢走了：ok=%v err=%v", ok, err)
	}
}

// TestExpiredLeaseCanBeStolenAndOldEpochIsLockedOut——⭐ 本文件的核心。
// 租约过期后新 worker 抢到，旧 worker 的 epoch 立刻失效；它此后的任何续租
// 都必须失败，因为那是它唯一能发现自己已经出局的信号。
func TestExpiredLeaseCanBeStolenAndOldEpochIsLockedOut(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-steal", "job-steal")

	old, ok, err := repo.claimExtractionJob(ctx, job.ID, -time.Second) // 立刻过期
	if err != nil || !ok {
		t.Fatalf("首次抢占失败：%v", err)
	}
	newEpoch, ok, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil || !ok {
		t.Fatalf("过期租约本该能被抢走：ok=%v err=%v", ok, err)
	}
	if newEpoch <= old {
		t.Errorf("抢占后 epoch 没有增长：%d -> %d", old, newEpoch)
	}
	held, err := repo.renewExtractionLease(ctx, job.ID, old, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("旧 epoch 还能续租——旧 worker 会一直以为自己是持有者")
	}
	if held, err := repo.renewExtractionLease(ctx, job.ID, newEpoch, time.Minute); err != nil || !held {
		t.Errorf("当前持有者续租失败：held=%v err=%v", held, err)
	}
}

// TestLeaseKeeperReportsLoss——⭐ 持有者必须能**主动发现**自己出局，
// 而不是等到写数据被拒。发现得越晚，白花的模型调用越多。
func TestLeaseKeeperReportsLoss(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-keeper", "job-keeper")

	epoch, ok, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	k := startLeaseKeeper(ctx, repo, job.ID, epoch, time.Minute, 10*time.Millisecond)
	defer k.Stop()

	if !k.Valid() {
		t.Fatal("刚抢到就报失效")
	}
	// 另一个 worker 把租约抢走（直接改数据库，模拟租约过期后的抢占）。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET epoch = epoch + 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-k.Lost():
	case <-time.After(3 * time.Second):
		t.Fatal("租约被抢走后 keeper 没有在心跳周期内发现")
	}
	if k.Valid() {
		t.Error("已经出局却仍然报有效")
	}
}

// TestLeaseKeeperStopsWhenJobIsNoLongerCurrent——文档指向了另一个 run，
// 或者文档改了版本，当前 worker 必须停下。
// ⚠️ 只看 epoch 是不够的：restart 会建一个新 job 并把文档指针改过去，
// 而**旧 job 的 epoch 一个字都没变**，旧 worker 会心安理得地继续跑完，
// 把一批属于旧版本的关系发布出去。
func TestLeaseKeeperStopsWhenJobIsNoLongerCurrent(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-super", "job-super")

	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	k := startLeaseKeeper(ctx, repo, job.ID, epoch, time.Minute, 10*time.Millisecond)
	defer k.Stop()

	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET active_relation_job_id = 'some-other-job' WHERE id = 'doc-super'`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-k.Lost():
	case <-time.After(3 * time.Second):
		t.Fatal("文档已指向别的 run，keeper 没有发现")
	}
}

// TestLeaseKeeperStopIsSynchronous——⭐ Stop 必须**等 goroutine 真的退出**。
// 宪法：每个 goroutine 必须明确谁负责等它结束。
// 不等的后果是测试里数据库连接已经关了、心跳还在写，表现为随机的
// "use of closed connection"——一个只在 CI 上偶发的、看不出根因的失败。
func TestLeaseKeeperStopIsSynchronous(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-stop", "job-stop")

	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	k := startLeaseKeeper(ctx, repo, job.ID, epoch, time.Minute, time.Millisecond)
	time.Sleep(20 * time.Millisecond) // 让心跳真的跑几轮
	k.Stop()

	// Stop 返回后不允许再有任何心跳写入：记下 heartbeat_at 再等一会儿。
	var before time.Time
	if err := repo.db.QueryRowContext(ctx,
		`SELECT heartbeat_at FROM relation_extraction_jobs WHERE id=?`, job.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	var after time.Time
	if err := repo.db.QueryRowContext(ctx,
		`SELECT heartbeat_at FROM relation_extraction_jobs WHERE id=?`, job.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Error("Stop 返回之后心跳仍在写——goroutine 没被等到")
	}
	// 重复 Stop 必须安全。
	k.Stop()
}

// TestLeaseKeeperStopsOnContextCancel——上下文取消（进程退出/用户暂停）时
// keeper 自己收摊，不需要调用方额外做什么。
func TestLeaseKeeperStopsOnContextCancel(t *testing.T) {
	repo := extractionRepo(t)
	job := claimTestJob(t, repo, "doc-cancel", "job-cancel")

	ctx, cancel := context.WithCancel(t.Context())
	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	k := startLeaseKeeper(ctx, repo, job.ID, epoch, time.Minute, 10*time.Millisecond)
	cancel()
	select {
	case <-k.Lost():
	case <-time.After(3 * time.Second):
		t.Fatal("上下文取消后 keeper 没有停下")
	}
	k.Stop()
}

// TestWorkLoopStopsCallingTheModelOnceInvalid——⭐ T016 真正要保证的东西。
// 前面几条证明 keeper 能**发现**失效；这一条证明工作循环会**据此停手**。
// ⚠️ 失效之后每多打一次模型调用，就是一笔白花的钱，且账目上找不出异常。
func TestWorkLoopStopsCallingTheModelOnceInvalid(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job := claimTestJob(t, repo, "doc-loop", "job-loop")

	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	k := startLeaseKeeper(ctx, repo, job.ID, epoch, time.Minute, 5*time.Millisecond)
	defer k.Stop()

	var calls atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			if !k.Valid() {
				return
			}
			calls.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET epoch = epoch + 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	<-k.Lost()
	settled := calls.Load()
	wg.Wait()
	if got := calls.Load(); got > settled+2 {
		t.Errorf("失效后又打了 %d 次调用（Lost 时 %d，最终 %d）", got-settled, settled, got)
	}
	if settled == 0 {
		t.Fatal("循环一次都没跑，这条断言等于没验")
	}
}

// --- keeper 的故障路径（变异测试逼出来的两个缺口）---

// fakeLeaseOwner 让两条只在故障时才走到的路径可测。
type fakeLeaseOwner struct {
	renews  atomic.Int64
	fail    atomic.Bool   // 让续租返回错误
	held    atomic.Bool   // 续租是否仍然属于自己
	current atomic.Bool   // 是否仍是文档当前作业
	block   chan struct{} // 非 nil 时让一次续租卡在这里
	entered chan struct{}
}

func newFakeLeaseOwner() *fakeLeaseOwner {
	o := &fakeLeaseOwner{}
	o.held.Store(true)
	o.current.Store(true)
	return o
}

func (o *fakeLeaseOwner) renewExtractionLease(ctx context.Context, _ string, _ int, _ time.Duration) (bool, error) {
	o.renews.Add(1)
	if o.block != nil {
		select {
		case o.entered <- struct{}{}:
		default:
		}
		<-o.block
	}
	if o.fail.Load() {
		return false, errors.New("fake: database unavailable")
	}
	return o.held.Load(), nil
}

func (o *fakeLeaseOwner) jobIsStillCurrent(context.Context, string) (bool, error) {
	return o.current.Load(), nil
}

// TestKeeperToleratesTransientRenewFailures——⭐ 一次续租失败**不**判出局。
//
// ⚠️ 数据库瞬时抖动很常见，而误判出局会白白丢掉一个正在正常工作的 worker：
// 它手上那次模型调用的钱已经花了，结果却被丢弃，然后另一个 worker 从头再来。
// 心跳频率是 TTL 的 1/6，允许连丢 5 次仍不丢租约；真的一直失败，
// 租约会自然过期、由别人接手——那条路是安全的，误判才不是。
func TestKeeperToleratesTransientRenewFailures(t *testing.T) {
	o := newFakeLeaseOwner()
	o.fail.Store(true)
	k := startLeaseKeeper(t.Context(), o, "job-x", 1, time.Minute, 2*time.Millisecond)
	defer k.Stop()

	// 连续失败若干轮，仍然必须认为自己是持有者。
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if o.renews.Load() >= 5 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n := o.renews.Load(); n < 5 {
		t.Fatalf("只续租了 %d 次，夹具没跑够", n)
	}
	if !k.Valid() {
		t.Error("连续几次续租失败就判出局了——一次数据库抖动不该丢掉正常 worker")
	}
	select {
	case <-k.Lost():
		t.Error("续租失败不该关闭 Lost()")
	default:
	}
}

// TestKeeperStopWaitsForInFlightRenew——⭐ Stop 必须等到**正在进行的**那次
// 续租返回，不是等到 goroutine 下一次 select。
//
// ⚠️ 不等的后果是调用方已经收摊、心跳还在写数据库，表现为随机的
// "use of closed connection"——一个只在 CI 上偶发、看不出根因的失败。
// 上一版这条断言太弱（Stop 后睡一会儿看有没有新写入），而没有 wg.Wait()
// 的实现照样能过：goroutine 下一轮 select 就退出了，本来也不会再写。
func TestKeeperStopWaitsForInFlightRenew(t *testing.T) {
	o := newFakeLeaseOwner()
	o.block = make(chan struct{})
	o.entered = make(chan struct{}, 1)
	k := startLeaseKeeper(t.Context(), o, "job-x", 1, time.Minute, time.Millisecond)

	<-o.entered // 确认一次续租已经进去了并卡住

	stopped := make(chan struct{})
	go func() {
		k.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("续租还卡在半路，Stop 就返回了——goroutine 没被等到")
	case <-time.After(50 * time.Millisecond):
	}
	close(o.block)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("续租返回后 Stop 仍未返回")
	}
}

// --- 010 R6-04：续租一直失败超过 TTL 之后必须判失效 ---

// alwaysFailingOwner 的续租永远报错，当前性检查永远说"还是你的"。
type alwaysFailingOwner struct{ renews atomic.Int32 }

func (o *alwaysFailingOwner) renewExtractionLease(ctx context.Context, jobID string, epoch int, ttl time.Duration) (bool, error) {
	o.renews.Add(1)
	return false, errors.New("database is down")
}

func (o *alwaysFailingOwner) jobIsStillCurrent(ctx context.Context, jobID string) (bool, error) {
	return true, nil
}

// TestKeeperGoesInvalidOnceTheLeaseCouldHaveExpired——⭐ 判据是**最后一次
// 确认成功的到期时间**，不是失败次数。
//
// ⚠️ 此前每次续租失败都直接 continue，于是数据库长期故障时 Valid() 可以
// 一直是 true——而租约早就过期、作业已经被别人接手。旧 worker 仍在调用
// 模型，每一次都是白花的钱，且账目上完全看不出异常。
func TestKeeperGoesInvalidOnceTheLeaseCouldHaveExpired(t *testing.T) {
	owner := &alwaysFailingOwner{}
	ttl := 60 * time.Millisecond
	k := startLeaseKeeper(t.Context(), owner, "job-r604", 1, ttl, ttl/6)
	defer k.Stop()

	// TTL 之内还应当是有效的：一次抖动不该误判出局。
	time.Sleep(ttl / 3)
	if !k.Valid() {
		t.Error("刚失败一两次就判出局了——误判会白白丢掉一个正常工作的 worker")
	}

	select {
	case <-k.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("续租一直失败、租约早已过期，keeper 仍然声称自己有效")
	}
	if k.Valid() {
		t.Error("Lost 已关闭，Valid 却仍然是 true")
	}
	if owner.renews.Load() == 0 {
		t.Error("根本没有尝试过续租，用例没测到想测的东西")
	}
}

// hangingOwner 的续租一直卡住，直到 ctx 被取消。
type hangingOwner struct{ maxBlocked atomic.Int64 }

func (o *hangingOwner) renewExtractionLease(ctx context.Context, jobID string, epoch int, ttl time.Duration) (bool, error) {
	start := time.Now()
	<-ctx.Done()
	if d := time.Since(start).Milliseconds(); d > o.maxBlocked.Load() {
		o.maxBlocked.Store(d)
	}
	return false, ctx.Err()
}

func (o *hangingOwner) jobIsStillCurrent(ctx context.Context, jobID string) (bool, error) {
	return true, nil
}

// TestKeeperBoundsEachRenewalByTheRemainingLease——⭐ 单次续租必须限制在
// **剩余租约期限之内**。
//
// ⚠️ 不限的话，一次卡住的续租可以远远超过 TTL 才返回，而这期间 Valid()
// 一直是 true——租约早被别人抢走了，而这个 worker 还在调模型。
func TestKeeperBoundsEachRenewalByTheRemainingLease(t *testing.T) {
	owner := &hangingOwner{}
	ttl := 60 * time.Millisecond
	k := startLeaseKeeper(t.Context(), owner, "job-r604b", 1, ttl, ttl/6)
	// ⚠️ **不能** defer k.Stop()：Stop 会等心跳 goroutine 退出，而缺了
	// 单次续租的时限时它正卡在 renew 里出不来——测试会挂死而不是失败。
	// 挂死在变异测试里长得像"逃逸"（没有 FAIL 行），这一条注释是为了
	// 下一个人不要再把它改回 defer。
	defer func() {
		done := make(chan struct{})
		go func() { k.Stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Stop 没能在 1 秒内返回——心跳 goroutine 卡在续租里")
		}
	}()

	select {
	case <-k.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("续租一直卡住，keeper 仍然声称自己有效")
	}
	if blocked := owner.maxBlocked.Load(); blocked > ttl.Milliseconds()*2 {
		t.Errorf("单次续租被卡了 %dms，超过了租约期限 %dms 的两倍——"+
			"这段时间里 Valid() 一直是 true", blocked, ttl.Milliseconds())
	}
}

// TestKeeperStaysValidWhileRenewalsSucceed——底线用例：续租正常时不能
// 因为新加的到期判断而误判出局。
//
// ⚠️ 少了它，一个"到点就判失效"的实现能让上面两条都通过。
func TestKeeperStaysValidWhileRenewalsSucceed(t *testing.T) {
	owner := &okOwner{}
	ttl := 60 * time.Millisecond
	k := startLeaseKeeper(t.Context(), owner, "job-r604c", 1, ttl, ttl/6)
	defer k.Stop()

	time.Sleep(ttl * 3)
	if !k.Valid() {
		t.Error("续租一直成功却被判出局了")
	}
	select {
	case <-k.Lost():
		t.Error("续租一直成功，Lost 却关闭了")
	default:
	}
}

type okOwner struct{}

func (o *okOwner) renewExtractionLease(ctx context.Context, jobID string, epoch int, ttl time.Duration) (bool, error) {
	return true, nil
}

func (o *okOwner) jobIsStillCurrent(ctx context.Context, jobID string) (bool, error) {
	return true, nil
}
